package session

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/mmcdole/rune/text"
)

// Session logging (lua.Host implementation). The file handle is
// Go-owned so an active log survives /reload; Run's defer closes it on
// exit. All methods run on the session goroutine (called from Lua), so
// no locking is needed.

// LogStart implements lua.Host. Opens path in append mode, creating
// parent directories. An already-open log is closed and replaced.
func (s *Session) LogStart(path string) (string, error) {
	path = expandHome(path)
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	if s.logFile != nil {
		s.logFile.Close()
	}
	s.logFile = f
	s.logPath = abs
	return abs, nil
}

// LogStop implements lua.Host.
func (s *Session) LogStop() bool {
	if s.logFile == nil {
		return false
	}
	s.logFile.Close()
	s.logFile = nil
	s.logPath = ""
	return true
}

// LogWrite implements lua.Host. A write failure (disk full, file
// deleted) closes the log and reports once, rather than erroring on
// every subsequent line.
func (s *Session) LogWrite(line string) {
	if s.logFile == nil {
		return
	}
	if _, err := s.logFile.WriteString(line + "\n"); err != nil {
		path := s.logPath
		s.LogStop()
		s.ui.Print(text.Red(fmt.Sprintf("[Log] write to %s failed (%v) - logging stopped", path, err)))
	}
}

// LogStatus implements lua.Host.
func (s *Session) LogStatus() (string, bool) {
	return s.logPath, s.logFile != nil
}

// maxLogLineBytes caps one log line during a read-back scan. Agent
// reasoning lines are far longer than MUD output, so bufio.Scanner's
// 64KB default is raised well past anything a turn could produce; a
// line beyond even this surfaces as an error rather than silently
// truncating the caller's view of its own log.
const maxLogLineBytes = 1 << 20

// scanLog opens the active log for reading and feeds every line to
// visit. The write handle is append-only and unbuffered (see LogWrite),
// so a second read-only handle observes every line written so far
// without any flush coordination.
func (s *Session) scanLog(visit func(line string)) error {
	if s.logFile == nil {
		return fmt.Errorf("no log is open")
	}
	f, err := os.Open(s.logPath)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	for sc.Scan() {
		visit(sc.Text())
	}
	return sc.Err()
}

// tail collects the last max lines seen by a scan. A fixed ring
// indexed by a running count keeps this O(lines) - shifting a slice
// per line would be O(lines x max), which on a long-running bot's log
// is exactly the kind of work the watchdog exists to catch.
type tail struct {
	buf   []string
	count int
}

func newTail(max int) *tail { return &tail{buf: make([]string, max)} }

func (t *tail) add(line string) {
	t.buf[t.count%len(t.buf)] = line
	t.count++
}

// lines returns what was kept, oldest first.
func (t *tail) lines() []string {
	n := t.count
	if n > len(t.buf) {
		n = len(t.buf)
	}
	out := make([]string, 0, n)
	for i := t.count - n; i < t.count; i++ {
		out = append(out, t.buf[i%len(t.buf)])
	}
	return out
}

// LogRead implements lua.Host.
func (s *Session) LogRead(maxLines int) ([]string, error) {
	if maxLines <= 0 {
		return nil, fmt.Errorf("maxLines must be positive, got %d", maxLines)
	}
	t := newTail(maxLines)
	if err := s.scanLog(t.add); err != nil {
		return nil, err
	}
	return t.lines(), nil
}

// LogSearch implements lua.Host. pattern is a Go regexp, the same
// engine and syntax triggers and aliases already use.
func (s *Session) LogSearch(pattern string, maxResults int) ([]string, error) {
	if maxResults <= 0 {
		return nil, fmt.Errorf("maxResults must be positive, got %d", maxResults)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}
	t := newTail(maxResults)
	if err := s.scanLog(func(line string) {
		if re.MatchString(line) {
			t.add(line)
		}
	}); err != nil {
		return nil, err
	}
	return t.lines(), nil
}

func expandHome(path string) string {
	if len(path) > 0 && path[0] == '~' {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}
