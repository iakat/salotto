package facade

import (
	"strings"
	"testing"
	"time"
)

// searchFixture sets up a bouncer whose log directory holds a day of history the client was never
// sent. That distinction is the whole point: a test whose hit also sits in the ring can pass while
// the facade answers from memory, which is the bug this feature exists to remove.
func searchFixture(t *testing.T, lines []string) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	writeLogDay(t, dir, "libera", "#c", day, lines)
	fn := newFakeNet("libera")
	cfg := testConfig(fn)
	cfg.LogDir = dir
	return cfg, day
}

// searchOverWire registers a client, drains its replay, sends one SEARCH and returns the raw
// answer.
func searchOverWire(t *testing.T, cfg Config, request string) string {
	t.Helper()
	_, addr := startServer(t, cfg)
	conn, br := dialClient(t, addr)
	registerWithCaps(t, conn, br, "me", "server-time batch")
	replay := collectReplay(t, conn, br)
	for _, line := range replay {
		if strings.Contains(line, "needle") || strings.Contains(line, "on disk") {
			t.Fatalf("the fixture leaked the hit into the replay, so the test cannot tell disk from memory: %q", line)
		}
	}
	conn.Write([]byte(request + "\r\n"))
	return drainFor(t, conn, br, 600*time.Millisecond)
}

// searchText returns the NOTICE text of every SEARCH line in an answer, tags and prefix removed.
func searchText(answer string) []string {
	var out []string
	for _, line := range strings.Split(answer, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		line = withoutTags(line)
		i := strings.Index(line, " :SEARCH")
		if i < 0 {
			continue
		}
		out = append(out, line[i+2:])
	}
	return out
}

// TestSearchReachesTheIndexNotJustTheClient: a line that exists only in a log file is returned by
// SEARCH. The mutation that must kill this is answering from the ring (or dropping the Query call)
// - both leave an answer that looks plausible and omits the only history the user asked for.
func TestSearchReachesTheIndexNotJustTheClient(t *testing.T) {
	cfg, day := searchFixture(t, []string{
		"[10:00:00] <alice> the needle on disk",
		"[10:01:00] <bob> unrelated chatter",
	})
	answer := searchOverWire(t, cfg, "SEARCH id:1 needle")

	lines := searchText(answer)
	if len(lines) != 2 {
		t.Fatalf("got %d SEARCH lines, want one hit and one terminator:\n%s", len(lines), answer)
	}
	if !strings.HasPrefix(lines[0], "SEARCH "+day+" 10:00:00 libera #c ") ||
		!strings.HasSuffix(lines[0], "<alice> the needle on disk") {
		t.Fatalf("the logged hit did not come back intact: %q", lines[0])
	}
	if !strings.Contains(answer, "salotto.search=1") {
		t.Fatalf("the answer does not carry the request id, so a late reply cannot be dropped:\n%s", answer)
	}
}

// TestSearchWithLoggingDisabledRefuses: with no log directory there is no index to search, and an
// empty result would be a lie - it looks exactly like "no hits". The client must get a reason.
func TestSearchWithLoggingDisabledRefuses(t *testing.T) {
	fn := newFakeNet("libera")
	cfg := testConfig(fn)
	cfg.LogDir = ""
	answer := searchOverWire(t, cfg, "SEARCH id:2 needle")

	lines := searchText(answer)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "SEARCH-ERR ") {
		t.Fatalf("disabled logging did not refuse with a reason: %q", answer)
	}
	if !strings.Contains(lines[0], "logging disabled") {
		t.Fatalf("the refusal does not say why: %q", lines[0])
	}
	if strings.Contains(answer, "SEARCH-END") {
		t.Fatalf("a failed search also sent a result terminator, which reads as an empty answer:\n%s", answer)
	}
}

// TestSearchEndCarriesTheCount: the terminator's number is the number of hit lines actually
// written. A hard-coded 0, or no terminator at all, leaves a client unable to tell a finished
// search from a slow one.
func TestSearchEndCarriesTheCount(t *testing.T) {
	cfg, _ := searchFixture(t, []string{
		"[10:00:00] <alice> needle one",
		"[10:01:00] <bob> needle two",
		"[10:02:00] <carol> needle three",
	})
	answer := searchOverWire(t, cfg, "SEARCH id:3 needle")

	lines := searchText(answer)
	hits := 0
	end := ""
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "SEARCH "):
			hits++
		case strings.HasPrefix(line, "SEARCH-END "):
			end = line
		}
	}
	if hits != 3 {
		t.Fatalf("got %d hit lines, want 3:\n%s", hits, answer)
	}
	if end != "SEARCH-END 3" {
		t.Fatalf("terminator = %q, want %q", end, "SEARCH-END 3")
	}
}

// TestSearchBodySurvivesSpacesAndColons: the body is the tail, and it must arrive byte for byte.
// Splitting the body after the fixed fields and rejoining it, or escaping it, changes the text the
// user searched for - and a tab is the case a naive whitespace parser loses first.
func TestSearchBodySurvivesSpacesAndColons(t *testing.T) {
	const body = "<alice> see http://x:y\tand more: yes"
	cfg, _ := searchFixture(t, []string{"[10:00:00] " + body})
	answer := searchOverWire(t, cfg, "SEARCH id:4 http")

	lines := searchText(answer)
	if len(lines) < 1 || !strings.HasPrefix(lines[0], "SEARCH ") {
		t.Fatalf("no hit came back:\n%s", answer)
	}
	rest := strings.TrimPrefix(lines[0], "SEARCH ")
	fields := strings.SplitN(rest, " ", 5)
	if len(fields) != 5 {
		t.Fatalf("reply did not split into five fields: %q", lines[0])
	}
	if fields[4] != body {
		t.Fatalf("body came back as %q, want %q", fields[4], body)
	}
}

// TestSearchWindowOptionScopesTheHits: window: is not decoration. Without it the facade searches
// every window on every network, which is the right default and the wrong answer when the user
// named one; a hit from a different window is as misleading as a hit from a different query.
func TestSearchWindowOptionScopesTheHits(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	writeLogDay(t, dir, "libera", "#a", day, []string{"[10:00:00] <alice> needle in a"})
	writeLogDay(t, dir, "libera", "#b", day, []string{"[10:01:00] <bob> needle in b"})
	cfg := testConfig(newFakeNet("libera"))
	cfg.LogDir = dir

	answer := searchOverWire(t, cfg, "SEARCH id:7 window:#a needle")
	lines := searchText(answer)
	hits := 0
	for _, line := range lines {
		if !strings.HasPrefix(line, "SEARCH ") {
			continue
		}
		hits++
		if !strings.Contains(line, " libera #a ") {
			t.Fatalf("a window-scoped search returned another window: %q", line)
		}
	}
	if hits != 1 {
		t.Fatalf("got %d hits, want the one in #a:\n%s", hits, answer)
	}
}

// TestSearchRefusesAPathInWindow: a client-supplied component reaches the filesystem, so it is
// validated at the boundary. Query would refuse it too, but only after a path has been built from
// client input, and the refusal here names the field that was wrong.
func TestSearchRefusesAPathInWindow(t *testing.T) {
	cfg, _ := searchFixture(t, []string{"[10:00:00] <alice> needle"})

	for _, tc := range []struct{ request, field string }{
		{"SEARCH id:5 window:../etc needle", "bad window"},
		{"SEARCH id:6 network:../../tmp needle", "bad network"},
	} {
		answer := searchOverWire(t, cfg, tc.request)
		lines := searchText(answer)
		if len(lines) == 0 || !strings.HasPrefix(lines[0], "SEARCH-ERR ") {
			t.Fatalf("%q was not refused: %q", tc.request, answer)
		}
		if !strings.Contains(lines[0], tc.field) {
			t.Fatalf("%q was refused without naming %q: %q", tc.request, tc.field, lines[0])
		}
	}
}
