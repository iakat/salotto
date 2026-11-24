package facade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLogDay writes one day's log for a window in the format internal/logfile actually produces:
// "[HH:MM:SS] <nick> text", with "*** action" for the system lines. The first version of this
// fixture used "nick<TAB>text", which is what I assumed the format was rather than what it is -
// and a parser written against the same assumption would have passed with it while reading nothing
// real. The format below is copied from a live log file.
func writeLogDay(t *testing.T, dir, network, window, day string, lines []string) {
	t.Helper()
	path := filepath.Join(dir, network, window)
	if network == "" {
		path = filepath.Join(dir, window)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(path, day+".log"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHistoryComesFromTheLogsWhenTheRingHasNothingOlder is the feature katia asked for: scrollback
// that reaches past the restart.
//
// The ring is memory. It dies with the process, so a bouncer restarted an hour ago can answer
// "what happened in the last hour" and nothing else - and a CHATHISTORY BEFORE whose cursor is
// older than the ring's oldest line is a question the ring cannot answer. The logs can. Serving the
// empty batch instead tells the client its history ended at the restart, which is a lie about what
// was recorded, and it is the lie that makes scrollback feel shallow for no visible reason.
func TestHistoryComesFromTheLogsWhenTheRingHasNothingOlder(t *testing.T) {
	dir := t.TempDir()
	// A day of history the ring will not have.
	writeLogDay(t, dir, "libera", "#c", "2026-11-20", []string{
		"[09:15:00] <alice> yesterday's conversation",
		"[09:16:00] <bob> another old line",
	})

	fn := newFakeNet("libera")
	// The ring holds only this, which is newer than the cursor below.
	fn.add("#c", ":bob!b@h PRIVMSG #c :a fresh line", time.Now().UnixNano())
	cfg := testConfig(fn)
	cfg.LogDir = dir
	s, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	// A cursor in the past: older than the ring, inside the logged day.
	cursor := time.Date(2026, 11, 21, 0, 0, 0, 0, time.Local).UTC().Format("2006-01-02T15:04:05.000000Z")
	conn.Write([]byte("CHATHISTORY BEFORE #c timestamp=" + cursor + " 10\r\n"))
	out := drainFor(t, conn, br, 600*time.Millisecond)

	if !strings.Contains(out, "yesterday's conversation") {
		t.Fatalf("a cursor older than the ring returned no logged history; the ring answered and the disk was never asked:\n%q", out)
	}
	if !strings.Contains(out, "BATCH +chathistory-#c") || !strings.Contains(out, "BATCH -chathistory-#c") {
		t.Fatalf("logged history did not arrive in a batch, so a client cannot tell where it ends:\n%q", out)
	}
}

// TestLoggedHistoryIsInOrderAndAttributedToTheNickThatSaidIt - the properties the first version of
// these tests did not check, which is why four mutations survived it.
//
// "the text is somewhere in the output" is a weak assertion: it passes whether the batch reads
// oldest-first (as a client expects) or reversed, and whether the line is attributed to the nick
// who said it or to the bouncer. Both are things a reader notices immediately and a substring test
// never will.
func TestLoggedHistoryIsInOrderAndAttributedToTheNickThatSaidIt(t *testing.T) {
	dir := t.TempDir()
	writeLogDay(t, dir, "libera", "#c", "2026-11-20", []string{
		"[09:15:00] <alice> the first thing said",
		"[09:16:00] <bob> the second thing said",
		"[09:17:00] <alice> the third thing said",
	})

	fn := newFakeNet("libera")
	fn.add("#c", ":bob!b@h PRIVMSG #c :fresh", time.Now().UnixNano())
	cfg := testConfig(fn)
	cfg.LogDir = dir
	s, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	conn.Write([]byte("CHATHISTORY BEFORE #c timestamp=2026-11-21T00:00:00.000000Z 10\r\n"))

	out := drainFor(t, conn, br, 600*time.Millisecond)

	first := strings.Index(out, "the first thing said")
	second := strings.Index(out, "the second thing said")
	third := strings.Index(out, "the third thing said")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("not all logged lines arrived:\n%q", out)
	}
	if !(first < second && second < third) {
		t.Fatalf("logged history is not oldest-first: positions %d,%d,%d\n%q", first, second, third, out)
	}
	// Attribution: the wire line must name the sender the log recorded.
	//
	// The prefix is ":alice", not ":alice!user@host": a log line records the nick and the text and
	// nothing else, and inventing a userhost to make the prefix look complete would be inventing
	// data. A client renders the nick either way.
	if !strings.Contains(out, ":alice PRIVMSG #c :") {
		t.Fatalf("a logged line from alice is not attributed to her; the batch invents a sender:\n%q", out)
	}
	if strings.Contains(out, "the first thing said") && !strings.Contains(out, "PRIVMSG") {
		t.Fatalf("logged history did not arrive as conversation lines:\n%q", out)
	}
}

// TestTheCursorExcludesTheLineItNames - BEFORE means strictly older. A batch that includes the
// cursor's own line gives the client a duplicate every time it pages, so the same message appears
// twice: once as the oldest line it already had and once as the first line of the new page.
func TestTheCursorExcludesTheLineItNames(t *testing.T) {
	dir := t.TempDir()
	writeLogDay(t, dir, "libera", "#c", "2026-11-20", []string{
		"[09:15:00] <alice> older than the cursor",
		"[09:16:00] <bob> exactly at the cursor",
	})

	fn := newFakeNet("libera")
	fn.add("#c", ":bob!b@h PRIVMSG #c :fresh", time.Now().UnixNano())
	cfg := testConfig(fn)
	cfg.LogDir = dir
	s, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	// The cursor is exactly the 09:16 line - expressed in the clock the log was written on, which
	// is the process's local time because the log format carries no timezone. Writing the cursor
	// in UTC here and the log in local time put them an hour apart and made this test assert the
	// wrong thing.
	cursor := time.Date(2026, 11, 20, 9, 16, 0, 0, time.Local).UTC().Format("2006-01-02T15:04:05.000000Z")
	conn.Write([]byte("CHATHISTORY BEFORE #c timestamp=" + cursor + " 10\r\n"))
	out := drainFor(t, conn, br, 600*time.Millisecond)
	if !strings.Contains(out, "older than the cursor") {
		t.Fatalf("the line before the cursor is missing:\n%q", out)
	}
	if strings.Contains(out, "exactly at the cursor") {
		t.Fatalf("the batch includes the cursor's own line, so paging duplicates it:\n%q", out)
	}
}

// TestTheRingIsPreferredForRecentHistory - the logs are the fallback, not the first answer. A line
// the ring holds must come back as the line the bouncer actually saw, not as a reconstruction from
// a log file: the ring has the original bytes, the log has a rendering of them.
func TestTheRingIsPreferredForRecentHistory(t *testing.T) {
	dir := t.TempDir()
	writeLogDay(t, dir, "libera", "#c", time.Now().Format("2006-01-02"), []string{
		"[10:00:00] <carol> this text came from the log file",
	})

	fn := newFakeNet("libera")
	fn.add("#c", ":alice!a@h PRIVMSG #c :this text came from the ring", time.Now().UnixNano())
	cfg := testConfig(fn)
	cfg.LogDir = dir
	s, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	conn.Write([]byte("CHATHISTORY LATEST #c * 10\r\n"))
	out := drainFor(t, conn, br, 500*time.Millisecond)
	if !strings.Contains(out, "this text came from the ring") {
		t.Fatalf("CHATHISTORY LATEST did not answer from the ring:\n%q", out)
	}
	if strings.Contains(out, "this text came from the log file") {
		t.Fatalf("the log answered a question the ring could answer, so recent history is a\nreconstruction rather than what happened:\n%q", out)
	}
}

// TestNoLogDirMeansNoFallback - with logging off there is nothing to read, and the answer must be
// the ring's (empty) batch rather than an error or a hang.
func TestNoLogDirMeansNoFallback(t *testing.T) {
	fn := newFakeNet("libera")
	fn.add("#c", ":bob!b@h PRIVMSG #c :fresh", time.Now().UnixNano())
	cfg := testConfig(fn)
	cfg.LogDir = "" // logging off
	s, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	conn.Write([]byte("CHATHISTORY BEFORE #c timestamp=2000-01-01T00:00:00.000000Z 10\r\n"))
	out := drainFor(t, conn, br, 500*time.Millisecond)
	// It must still answer with a batch: "no history" and "no answer" are different things and
	// only one of them is true.
	if !strings.Contains(out, "chathistory-#c") {
		t.Fatalf("with logging off, an old cursor got no batch at all:\n%q", out)
	}
}

// TestAWindowThatExistsOnlyInTheLogsCanStillBeAsked - the real restart case.
//
// After a restart the ring is empty, so the window does not exist in memory at all: no line has
// been buffered for it yet. A CHATHISTORY for that window got a 421, which the client reads as
// "this bouncer does not know that window" - even though the window's whole history is sitting on
// disk. That is the shape katia reported: scrollback that stops at the restart.
//
// The fix is to treat a window as known when either half knows it: the ring or the logs.
func TestAWindowThatExistsOnlyInTheLogsCanStillBeAsked(t *testing.T) {
	dir := t.TempDir()
	writeLogDay(t, dir, "libera", "#gone", "2026-11-20", []string{
		"[09:15:00] <alice> said long before the restart",
	})

	fn := newFakeNet("libera")
	// Deliberately no line for #gone: the ring has never heard of it.
	fn.add("status", ":srv 001 up :Welcome", time.Now().UnixNano())
	cfg := testConfig(fn)
	cfg.LogDir = dir
	s, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch chathistory")
	collectReplay(t, conn, br)
	waitFor(t, 3*time.Second, func() bool { return s.Attached() == 1 }, "client attached")

	conn.Write([]byte("CHATHISTORY LATEST #gone * 20\r\n"))
	out := drainFor(t, conn, br, 700*time.Millisecond)
	if strings.Contains(out, "421") {
		t.Fatalf("a window that exists only in the logs was refused as unknown:\n%q", out)
	}
	if !strings.Contains(out, "said long before the restart") {
		t.Fatalf("a window that exists only in the logs served nothing:\n%q", out)
	}
}
