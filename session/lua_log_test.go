package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLogCapturesSessionToFile drives a logged session end-to-end and
// verifies the file reads like the screen: ANSI-stripped output, the
// local echo of typed input, no gagged lines, start/stop stamps.
func TestLogCapturesSessionToFile(t *testing.T) {
	s, net, _ := newTestSession(t)
	net.connected = true

	path := filepath.Join(s.config.ConfigDir, "session.log")
	userInput(s, "/log start "+path)
	if _, active := s.LogStatus(); !active {
		t.Fatal("log not active after /log start")
	}

	if err := s.engine.DoString("gag",
		`rune.trigger.contains("secret", function() end, {gag=true})`); err != nil {
		t.Fatal(err)
	}

	serverLine(s, "Hello \x1b[31mred\x1b[0m world")
	serverLine(s, "a secret line")
	userInput(s, "kill rat")
	userInput(s, "/log stop")

	if _, active := s.LogStatus(); active {
		t.Fatal("log still active after /log stop")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "Hello red world") {
		t.Errorf("expected ANSI-stripped output line in log, got:\n%s", content)
	}
	if strings.Contains(content, "\x1b") {
		t.Errorf("log contains raw ANSI escapes:\n%q", content)
	}
	if !strings.Contains(content, "> kill rat") {
		t.Errorf("expected echoed input in log, got:\n%s", content)
	}
	if strings.Contains(content, "secret") {
		t.Errorf("gagged line leaked into log:\n%s", content)
	}
	if !strings.Contains(content, "--- Log started") ||
		!strings.Contains(content, "--- Log stopped") {
		t.Errorf("expected start/stop stamps, got:\n%s", content)
	}
}

// TestLogDefaultPathUnderConfigDir verifies /log start with no
// argument creates a timestamped file under <config>/logs/.
func TestLogDefaultPathUnderConfigDir(t *testing.T) {
	s, _, _ := newTestSession(t)

	userInput(s, "/log start")
	path, active := s.LogStatus()
	if !active {
		t.Fatal("log not active after /log start")
	}
	wantPrefix := filepath.Join(s.config.ConfigDir, "logs") + string(filepath.Separator)
	if !strings.HasPrefix(path, wantPrefix) {
		t.Errorf("default log path = %q, want under %q", path, wantPrefix)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("default log file not created: %v", err)
	}
	userInput(s, "/log stop")
}

// TestLogSurvivesReload verifies the Go-owned file handle keeps
// logging across the Lua VM teardown of /reload.
func TestLogSurvivesReload(t *testing.T) {
	s, _, _ := newTestSession(t)

	path := filepath.Join(s.config.ConfigDir, "reload.log")
	userInput(s, "/log start "+path)

	s.Reload()
	cb := <-s.asyncResults // reload is deferred
	cb()

	if got, active := s.LogStatus(); !active || got != path {
		t.Fatalf("log did not survive reload: path=%q active=%v", got, active)
	}

	serverLine(s, "after reload")
	userInput(s, "/log stop")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log: %v", err)
	}
	if !strings.Contains(string(data), "after reload") {
		t.Errorf("line after reload missing from log:\n%s", data)
	}
}

// --- read-back (LogRead / LogSearch, see lua/core/92_agent_log.lua) ---

// writeLogLines opens a log and writes lines through the same
// Host.LogWrite path Lua uses, so read-back is tested against bytes
// that actually went to disk rather than a fixture file. The log is
// left open (read-back while writing is the case that matters) and
// closed in cleanup - Windows cannot remove t.TempDir's contents while
// a handle is still held.
func writeLogLines(t *testing.T, s *Session, lines ...string) string {
	t.Helper()
	path := filepath.Join(s.config.ConfigDir, "read.log")
	if _, err := s.LogStart(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.LogStop() })
	for _, line := range lines {
		s.LogWrite(line)
	}
	return path
}

func TestLogReadReturnsTrailingLinesOldestFirst(t *testing.T) {
	s, _, _ := newTestSession(t)
	writeLogLines(t, s, "one", "two", "three", "four")

	got, err := s.LogRead(2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"three", "four"}
	if len(got) != len(want) {
		t.Fatalf("LogRead(2) = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LogRead(2) = %q, want %q", got, want)
		}
	}
}

// A cap larger than the file must return everything, not pad or fail -
// the ring is sized by the cap, not by what is actually there.
func TestLogReadCapAboveFileLength(t *testing.T) {
	s, _, _ := newTestSession(t)
	writeLogLines(t, s, "only", "two")

	got, err := s.LogRead(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "only" || got[1] != "two" {
		t.Fatalf("LogRead(100) = %q, want [only two]", got)
	}
}

// The ring must survive wrapping many times over, which the naive
// slice-shift implementation would also pass but far more slowly -
// this pins the ordering, which is the part that is easy to get wrong.
func TestLogReadRingWrapsCorrectly(t *testing.T) {
	s, _, _ := newTestSession(t)
	var lines []string
	for i := 0; i < 500; i++ {
		lines = append(lines, fmt.Sprintf("line-%d", i))
	}
	writeLogLines(t, s, lines...)

	got, err := s.LogRead(3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line-497", "line-498", "line-499"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LogRead(3) after wrapping = %q, want %q", got, want)
		}
	}
}

// Searching a live log should surface what happened *recently*, so a
// capped search keeps the newest matches and discards the oldest.
func TestLogSearchKeepsMostRecentMatches(t *testing.T) {
	s, _, _ := newTestSession(t)
	writeLogLines(t, s,
		"kobold appears", "you miss", "kobold dies",
		"rat appears", "kobold returns")

	got, err := s.LogSearch("kobold", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kobold dies", "kobold returns"}
	if len(got) != len(want) {
		t.Fatalf("LogSearch = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LogSearch = %q, want %q", got, want)
		}
	}
}

func TestLogSearchNoMatchesIsEmptyNotError(t *testing.T) {
	s, _, _ := newTestSession(t)
	writeLogLines(t, s, "nothing interesting")

	got, err := s.LogSearch("dragon", 10)
	if err != nil {
		t.Fatalf("a search with no hits should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("LogSearch = %q, want no results", got)
	}
}

func TestLogReadAndSearchRequireAnOpenLog(t *testing.T) {
	s, _, _ := newTestSession(t)

	if _, err := s.LogRead(10); err == nil {
		t.Error("LogRead with no open log should error")
	}
	if _, err := s.LogSearch("x", 10); err == nil {
		t.Error("LogSearch with no open log should error")
	}
}

func TestLogSearchRejectsInvalidPattern(t *testing.T) {
	s, _, _ := newTestSession(t)
	writeLogLines(t, s, "a line")

	if _, err := s.LogSearch("[unclosed", 10); err == nil {
		t.Error("LogSearch should reject an invalid regexp")
	}
}

// Read-back sees lines written moments earlier through the still-open
// append handle - the property that makes an agent able to search its
// own log while it is being written.
func TestLogReadSeesWritesToStillOpenLog(t *testing.T) {
	s, _, _ := newTestSession(t)
	writeLogLines(t, s, "first")

	if got, err := s.LogRead(10); err != nil || len(got) != 1 {
		t.Fatalf("LogRead = %q, %v; want 1 line", got, err)
	}

	s.LogWrite("second")
	got, err := s.LogRead(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "second" {
		t.Fatalf("LogRead after a further write = %q, want the new line included", got)
	}
}
