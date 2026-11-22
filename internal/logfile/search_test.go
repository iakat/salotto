package logfile

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupLogs(t *testing.T) string {
	dir := t.TempDir()
	netDir := filepath.Join(dir, "net1", "#chan")
	os.MkdirAll(netDir, 0755)
	os.WriteFile(filepath.Join(netDir, "2026-11-20.log"), []byte("[10:00:00] hello world\n[10:00:01] drugs here\n"), 0644)
	os.WriteFile(filepath.Join(netDir, "2026-11-21.log"), []byte("[11:00:00] another line\n"), 0644)
	return dir
}

func TestSearchHTTPBasic(t *testing.T) {
	dir := setupLogs(t)
	h := SearchHandler(dir, "secret")
	// missing index -> scan fallback
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/log/search?network=net1&window=%23chan&q=hello&since=2026-11-20&until=2026-11-21", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "hello world") {
		t.Fatalf("body: %s", rr.Body.String())
	}
}

func TestSearchHTTPTraversal(t *testing.T) {
	dir := setupLogs(t)
	h := SearchHandler(dir, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/log/search?network=net1&window=..%2F..&q=x", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("code %d", rr.Code)
	}
}

func TestSearchHTTPBadToken(t *testing.T) {
	dir := setupLogs(t)
	h := SearchHandler(dir, "secret")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/log/search?network=net1&window=%23chan&q=hello", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("code %d", rr.Code)
	}
}

func TestSearchHTTPLimitOrder(t *testing.T) {
	dir := setupLogs(t)
	// build index for one file
	BuildIndexFile(filepath.Join(dir, "net1", "#chan", "2026-11-20.log"), filepath.Join(dir, "net1", "#chan", "2026-11-20.log")+".idx")
	h := SearchHandler(dir, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/log/search?network=net1&window=%23chan&q=hello&limit=1&order=asc", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code %d", rr.Code)
	}
}

func TestWindowsEndpoint(t *testing.T) {
	dir := setupLogs(t)
	h := SearchHandler(dir, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/log/windows?network=net1", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "net1 #chan") {
		t.Fatalf("body: %s", rr.Body.String())
	}
}

func TestBadDate(t *testing.T) {
	dir := setupLogs(t)
	h := SearchHandler(dir, "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/log/search?network=net1&window=%23chan&q=x&since=bad", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("code %d", rr.Code)
	}
}

func TestIndexedSearchPrefix(t *testing.T) {
	dir := setupLogs(t)
	BuildIndexFile(filepath.Join(dir, "net1", "#chan", "2026-11-20.log"), filepath.Join(dir, "net1", "#chan", "2026-11-20.log")+".idx")
	hits, err := Query(dir, "net1", "#chan", "drug", "2026-11-20", "2026-11-20", 10, "asc")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits: %+v", hits)
	}
}
