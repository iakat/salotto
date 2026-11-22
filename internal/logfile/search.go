package logfile

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/blevesearch/vellum"
)

type Hit struct {
	Date    string
	Time    string
	Network string
	Window  string
	Body    string
}

func extractTerms(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() >= 2 {
			out = append(out, b.String())
		}
		b.Reset()
	}
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	// dedupe not required
	return out
}

func BuildIndex(logPath, idxPath string) error { return BuildIndexFile(logPath, idxPath) }
func BuildIndexFile(logPath, idxPath string) error {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return err
	}
	type lineInfo struct {
		offset uint64
		text   []byte
	}
	var lines []lineInfo
	start := 0
	for i := 0; i <= len(data); i++ {
		if i == len(data) || data[i] == '\n' {
			if i > start {
				lines = append(lines, lineInfo{offset: uint64(start), text: data[start:i]})
			}
			start = i + 1
		}
	}
	postingsMap := map[string][]uint64{}
	for _, ln := range lines {
		for _, t := range extractTerms(string(ln.text)) {
			postingsMap[t] = append(postingsMap[t], ln.offset)
		}
	}
	terms := make([]string, 0, len(postingsMap))
	for t := range postingsMap {
		terms = append(terms, t)
	}
	sort.Strings(terms)

	var postings []byte
	termOffset := map[string]uint64{}
	buf := make([]byte, binary.MaxVarintLen64)
	for _, t := range terms {
		offs := postingsMap[t]
		termOffset[t] = uint64(len(postings))
		n := binary.PutUvarint(buf, uint64(len(offs)))
		postings = append(postings, buf[:n]...)
		var prev uint64
		for i, o := range offs {
			var delta uint64
			if i == 0 {
				delta = o
			} else {
				delta = o - prev
			}
			n := binary.PutUvarint(buf, delta)
			postings = append(postings, buf[:n]...)
			prev = o
		}
	}

	var fstBuf bytes.Buffer
	b, err := vellum.New(&fstBuf, nil)
	if err != nil {
		return err
	}
	for _, t := range terms {
		if err := b.Insert([]byte(t), termOffset[t]); err != nil {
			_ = b.Close()
			return err
		}
	}
	if err := b.Close(); err != nil {
		return err
	}
	fstBytes := fstBuf.Bytes()

	var out bytes.Buffer
	out.Write(postings)
	fstOffset := uint64(out.Len())
	out.Write(fstBytes)
	var u64buf [8]byte
	binary.LittleEndian.PutUint64(u64buf[:], fstOffset)
	out.Write(u64buf[:])
	var u32buf [4]byte
	binary.LittleEndian.PutUint32(u32buf[:], 0x53544f4c)
	out.Write(u32buf[:])
	out.WriteString("1")

	// Write atomically: the startup catch-up pass and lazy queries may build the
	// same index concurrently, and a reader must never see a half-written file.
	tmp, err := os.CreateTemp(filepath.Dir(idxPath), filepath.Base(idxPath)+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(out.Bytes()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, idxPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func BuildIndexForDay(logDir, network, window, day string) error {
	logPath := filepath.Join(logDir, network, window, day+".log")
	idxPath := logPath + ".idx"
	return BuildIndexFile(logPath, idxPath)
}

type Index struct {
	FST      *vellum.FST
	Postings []byte
}

func LoadIndex(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	n := len(data)
	if n < 13 {
		return nil, fmt.Errorf("index too short")
	}
	if data[n-1] != '1' {
		return nil, fmt.Errorf("bad index trailer")
	}
	magic := binary.LittleEndian.Uint32(data[n-5 : n-1])
	if magic != 0x53544f4c {
		return nil, fmt.Errorf("bad magic")
	}
	fstOffset := binary.LittleEndian.Uint64(data[n-13 : n-5])
	if fstOffset > uint64(n-13) {
		return nil, fmt.Errorf("bad fst offset")
	}
	fstBytes := data[fstOffset : n-13]
	postings := data[:fstOffset]
	fst, err := vellum.Load(fstBytes)
	if err != nil {
		return nil, err
	}
	return &Index{FST: fst, Postings: postings}, nil
}

func (idx *Index) Get(key []byte) (uint64, bool, error) {
	return idx.FST.Get(key)
}

func decodePostings(postings []byte, off uint64) ([]uint64, error) {
	if off >= uint64(len(postings)) {
		return nil, fmt.Errorf("offset out of range")
	}
	count, cn := binary.Uvarint(postings[off:])
	if cn <= 0 {
		return nil, fmt.Errorf("bad count")
	}
	pos := off + uint64(cn)
	out := make([]uint64, 0, count)
	var prev uint64
	for i := uint64(0); i < count; i++ {
		if pos >= uint64(len(postings)) {
			return nil, fmt.Errorf("truncated postings")
		}
		d, dn := binary.Uvarint(postings[pos:])
		if dn <= 0 {
			return nil, fmt.Errorf("bad delta")
		}
		pos += uint64(dn)
		var val uint64
		if i == 0 {
			val = d
		} else {
			val = prev + d
		}
		out = append(out, val)
		prev = val
	}
	return out, nil
}

func isBadPathComponent(s string) bool {
	if strings.ContainsAny(s, "/\\") {
		return true
	}
	if strings.Contains(s, "..") {
		return true
	}
	return false
}

func validateDate(s string) error {
	_, err := time.Parse("2006-01-02", s)
	return err
}

func safeJoin(logDir, network, window string) (string, bool) {
	if isBadPathComponent(network) {
		return "", false
	}
	if window != "" && isBadPathComponent(window) {
		return "", false
	}
	base := filepath.Clean(logDir)
	target := filepath.Clean(filepath.Join(base, network))
	if window != "" {
		target = filepath.Clean(filepath.Join(target, window))
	}
	if !strings.HasPrefix(target, base) {
		return "", false
	}
	return target, true
}

// Query returns hits. since/until YYYY-MM-DD inclusive; empty defaults today & 7 days back.
func Query(logDir, network, window, q, since, until string, limit int, order string) ([]Hit, error) {
	if logDir == "" {
		return nil, fmt.Errorf("logging disabled")
	}
	if isBadPathComponent(network) && network != "" {
		return nil, fmt.Errorf("bad network")
	}
	if window != "" && isBadPathComponent(window) {
		return nil, fmt.Errorf("bad window")
	}
	if _, ok := safeJoin(logDir, network, window); !ok {
		return nil, fmt.Errorf("bad path")
	}

	now := time.Now()
	if until == "" {
		until = now.Format("2006-01-02")
	}
	if since == "" {
		since = now.AddDate(0, 0, -7).Format("2006-01-02")
	}
	if err := validateDate(since); err != nil {
		return nil, fmt.Errorf("bad since: %w", err)
	}
	if err := validateDate(until); err != nil {
		return nil, fmt.Errorf("bad until: %w", err)
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	if order == "" {
		order = "desc"
	}

	// date list ascending
	startD, _ := time.Parse("2006-01-02", since)
	endD, _ := time.Parse("2006-01-02", until)
	var days []string
	for d := startD; !d.After(endD); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format("2006-01-02"))
	}

	qwords := strings.Fields(q)
	qterms := extractTerms(q)

	var hits []Hit

	// determine networks to search
	networks := []string{network}
	if network == "" {
		// all
		ents, _ := os.ReadDir(logDir)
		for _, e := range ents {
			if e.IsDir() {
				networks = append(networks, e.Name())
			}
		}
		if len(networks) > 1 {
			networks = networks[1:]
		}
	}

	for _, netName := range networks {
		if netName == "" {
			continue
		}
		if isBadPathComponent(netName) {
			continue
		}
		// windows to search
		windows := []string{window}
		if window == "" {
			ws, _ := Windows(logDir, netName)
			windows = ws
		}
		for _, wname := range windows {
			if wname == "" {
				continue
			}
			for _, day := range days {
				hs := searchFile(logDir, netName, wname, day, q, qwords, qterms)
				hits = append(hits, hs...)
			}
		}
	}

	// sort ascending by date,time,network,window,offset unknown; stable
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Date != hits[j].Date {
			return hits[i].Date < hits[j].Date
		}
		if hits[i].Time != hits[j].Time {
			return hits[i].Time < hits[j].Time
		}
		if hits[i].Network != hits[j].Network {
			return hits[i].Network < hits[j].Network
		}
		return hits[i].Window < hits[j].Window
	})

	if order == "desc" {
		// reverse
		for i, j := 0, len(hits)-1; i < j; i, j = i+1, j-1 {
			hits[i], hits[j] = hits[j], hits[i]
		}
	}

	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func searchFile(logDir, network, window, day, q string, qwords []string, qterms []string) []Hit {
	logPath := filepath.Join(logDir, network, window, day+".log")
	if _, err := os.Stat(logPath); err != nil {
		return nil
	}
	idxPath := logPath + ".idx"
	// Build on miss and rebuild when the log is newer. A missing .idx is a cache
	// miss to fill once, not an error to fall back from silently.
	fi, err := os.Stat(logPath)
	if err != nil {
		return nil
	}
	if ii, err := os.Stat(idxPath); err != nil || fi.ModTime().After(ii.ModTime()) {
		if err := BuildIndexFile(logPath, idxPath); err != nil {
			return scanFile(logPath, network, window, day, qwords)
		}
	}
	idx, err := LoadIndex(idxPath)
	if err == nil && idx != nil {
		return searchIndexed(idx, logPath, network, window, day, qwords, qterms)
	}
	return scanFile(logPath, network, window, day, qwords)
}

func searchIndexed(idx *Index, logPath, network, window, day string, qwords []string, qterms []string) []Hit {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	// candidate offsets
	var candidateSet map[uint64]bool
	if len(qterms) == 0 {
		// all lines
		candidateSet = map[uint64]bool{}
		start := 0
		for i := 0; i <= len(data); i++ {
			if i == len(data) || data[i] == '\n' {
				if i > start {
					candidateSet[uint64(start)] = true
				}
				start = i + 1
			}
		}
	} else {
		// for each qterm, find FST keys with prefix, collect postings
		termCandidates := make([]map[uint64]bool, 0, len(qterms))
		for _, qt := range qterms {
			set := map[uint64]bool{}
			itr, err := idx.FST.Iterator([]byte(qt), nil)
			if err != nil {
				continue
			}
			for {
				key, val := itr.Current()
				if len(key) == 0 && val == 0 {
					// maybe done
				}
				if !bytes.HasPrefix(key, []byte(qt)) {
					break
				}
				offs, err := decodePostings(idx.Postings, val)
				if err == nil {
					for _, o := range offs {
						set[o] = true
					}
				}
				if err := itr.Next(); err != nil {
					break
				}
			}
			termCandidates = append(termCandidates, set)
		}
		if len(termCandidates) == 0 {
			return nil
		}
		candidateSet = termCandidates[0]
		for _, s := range termCandidates[1:] {
			for off := range candidateSet {
				if !s[off] {
					delete(candidateSet, off)
				}
			}
		}
	}

	var hits []Hit
	for off := range candidateSet {
		// find line end
		end := off
		for end < uint64(len(data)) && data[end] != '\n' {
			end++
		}
		line := string(data[off:end])
		// verify all qwords
		ok := true
		lower := strings.ToLower(line)
		for _, w := range qwords {
			if !strings.Contains(lower, strings.ToLower(w)) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		timeStr, body := parseLogLine(line)
		hits = append(hits, Hit{Date: day, Time: timeStr, Network: network, Window: window, Body: body})
	}
	return hits
}

func scanFile(logPath, network, window, day string, qwords []string) []Hit {
	f, err := os.Open(logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var hits []Hit
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		lower := strings.ToLower(line)
		ok := true
		for _, w := range qwords {
			if !strings.Contains(lower, strings.ToLower(w)) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		timeStr, body := parseLogLine(line)
		hits = append(hits, Hit{Date: day, Time: timeStr, Network: network, Window: window, Body: body})
	}
	return hits
}

func parseLogLine(line string) (string, string) {
	line = strings.TrimRight(line, "\r")
	if strings.HasPrefix(line, "[") {
		if i := strings.Index(line, "]"); i > 0 {
			return line[1:i], strings.TrimSpace(line[i+1:])
		}
	}
	return "", line
}

func Windows(logDir, network string) ([]string, error) {
	netDir := filepath.Join(logDir, network)
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// SearchHandler returns an http.Handler serving the log search endpoints.
func SearchHandler(logDir, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/log/search", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if token != "" {
			auth := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(auth, prefix) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			got := strings.TrimSpace(auth[len(prefix):])
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		qp := r.URL.Query()
		network := qp.Get("network")
		window := qp.Get("window")
		q := qp.Get("q")
		since := qp.Get("since")
		until := qp.Get("until")
		limit := 0
		if s := qp.Get("limit"); s != "" {
			fmt.Sscan(s, &limit)
		}
		order := qp.Get("order")

		if network != "" && isBadPathComponent(network) {
			http.Error(w, "bad network", http.StatusBadRequest)
			return
		}
		if window != "" && isBadPathComponent(window) {
			http.Error(w, "bad window", http.StatusBadRequest)
			return
		}
		if since != "" {
			if err := validateDate(since); err != nil {
				http.Error(w, "bad since: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		if until != "" {
			if err := validateDate(until); err != nil {
				http.Error(w, "bad until: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		// path traversal via cleaned paths
		if network != "" {
			if _, ok := safeJoin(logDir, network, window); !ok {
				http.Error(w, "bad path", http.StatusBadRequest)
				return
			}
		}

		hits, err := Query(logDir, network, window, q, since, until, limit, order)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var b strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&b, "%s %s %s %s %s\n", h.Date, h.Time, h.Network, h.Window, h.Body)
		}
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/v1/log/windows", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if token != "" {
			auth := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(auth, prefix) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			got := strings.TrimSpace(auth[len(prefix):])
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		network := r.URL.Query().Get("network")
		if network == "" || isBadPathComponent(network) {
			http.Error(w, "bad network", http.StatusBadRequest)
			return
		}
		ws, err := Windows(logDir, network)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var b strings.Builder
		for _, win := range ws {
			fmt.Fprintf(&b, "%s %s\n", network, win)
		}
		_, _ = w.Write([]byte(b.String()))
	})
	return mux
}
