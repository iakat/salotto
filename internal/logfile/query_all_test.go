package logfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQueryWithNoSearchTermsReturnsLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "libera", "#c")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[09:15:00] <alice> one\n[09:16:00] <bob> two\n"
	if err := os.WriteFile(filepath.Join(p, "2026-11-20.log"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := Query(dir, "libera", "#c", "", "2026-11-20", "2026-11-20", 10, "desc")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("an empty q returned %d hits, want 2 - a history request has no search term and must still return lines", len(hits))
	}
	if hits[0].Time == "" {
		t.Fatalf("hits carry no time, so a cursor cannot be compared: %+v", hits[0])
	}
}
