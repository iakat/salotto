package logfile

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxLineBytes mirrors upstream's maxLine (8192) without importing that package:
// the logfile must not depend on upstream, and the constant is part of the
// wire/transport contract rather than something to share by import.
const maxLineBytes = 8192

type event struct {
	buf        *[]byte
	n          int
	out        bool
	connect    bool
	disconnect bool
	server     string
}

// Writer is a per-network log writer with a bounded queue and a drain goroutine.
type Writer struct {
	network string
	logDir  string
	nick    string
	server  string
	queue   chan event
	dropped atomic.Uint64

	pool sync.Pool

	tracker *Tracker
	clock   func() time.Time
	hasCap  func(string) bool

	mu       sync.Mutex
	file     *os.File
	fileDay  string
	filePath string

	// rollover indexing
	lastDay string

	// indexMu serialises the startup/catch-up index pass with itself and with
	// any explicit pass. bgMu guards the WaitGroup against Add after Close.
	indexMu sync.Mutex
	bgMu    sync.Mutex
	closed  bool

	done chan struct{}
	wg   sync.WaitGroup
}

func New(network, logDir string, queueDepth int, clock func() time.Time) *Writer {
	if clock == nil {
		clock = time.Now
	}
	if queueDepth <= 0 {
		queueDepth = 4096
	}
	w := &Writer{
		network: network,
		logDir:  logDir,
		queue:   make(chan event, queueDepth),
		tracker: NewTracker(""),
		clock:   clock,
		done:    make(chan struct{}),
	}
	w.pool.New = func() any {
		b := make([]byte, maxLineBytes)
		return &b
	}
	w.tracker.SetOwnNick("") // will be set by 001
	// allow nick injection via separate field set later; track initial via tracker only
	w.startIndexPass()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run()
	}()
	return w
}

// NewWriter alias.
func NewWriter(network, logDir string, queueDepth int, clock func() time.Time) *Writer {
	return New(network, logDir, queueDepth, clock)
}

func (w *Writer) SetNick(nick string) {
	w.nick = nick
	w.tracker.SetOwnNick(nick)
	// SetNick is where the network becomes usable; catch up on any log files
	// that were written before this process started.
	w.startIndexPass()
}
func (w *Writer) SetHasCap(fn func(string) bool) { w.hasCap = fn }
func (w *Writer) Tracker() *Tracker              { return w.tracker }

func (w *Writer) OnLine(seq uint64, line []byte) {
	_ = seq
	p := w.pool.Get().(*[]byte)
	if cap(*p) < len(line) {
		// oversized: fall back to a fresh slab-sized buffer (should not happen)
		b := make([]byte, len(line))
		p = &b
	}
	b := (*p)[:len(line)]
	copy(b, line)
	select {
	case w.queue <- event{buf: p, n: len(line)}:
	default:
		w.pool.Put(p)
		w.dropped.Add(1)
	}
}

func (w *Writer) OnSend(line []byte) {
	p := w.pool.Get().(*[]byte)
	if cap(*p) < len(line) {
		b := make([]byte, len(line))
		p = &b
	}
	b := (*p)[:len(line)]
	copy(b, line)
	select {
	case w.queue <- event{buf: p, n: len(line), out: true}:
	default:
		w.pool.Put(p)
		w.dropped.Add(1)
	}
}

func (w *Writer) Connect(server string) {
	select {
	case w.queue <- event{connect: true, server: server}:
	default:
		w.dropped.Add(1)
	}
}

func (w *Writer) Disconnect(server string) {
	select {
	case w.queue <- event{disconnect: true, server: server}:
	default:
		w.dropped.Add(1)
	}
}

func (w *Writer) Dropped() uint64 { return w.dropped.Load() }

func (w *Writer) Close() {
	w.bgMu.Lock()
	w.closed = true
	w.bgMu.Unlock()
	close(w.done)
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
}

func (w *Writer) run() {
	for {
		select {
		case e := <-w.queue:
			w.handle(e)
		case <-w.done:
			// drain remaining
			for {
				select {
				case e := <-w.queue:
					w.handle(e)
				default:
					w.closeFile()
					return
				}
			}
		}
	}
}

func (w *Writer) closeFile() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
}

func (w *Writer) SetServer(server string) { w.server = server }

func (w *Writer) handle(e event) {
	if e.buf != nil {
		p := e.buf
		defer w.pool.Put(p)
	}

	if w.logDir == "" && e.buf != nil {
		// No log files, but the tracker still backs per-window live routing
		// (logfile.WindowFor), so keep observing. Nothing to format or write.
		if !e.out {
			w.tracker.Observe((*e.buf)[:e.n])
		}
		return
	}

	now := w.clock()
	day := now.Format("2006-01-02")

	if e.connect {
		w.writeEntries(day, []Entry{{Window: "status", Body: fmt.Sprintf("Connected to IRC (%s)", e.server)}}, now)
		// use event server for connect body already
		return
	}
	if e.disconnect {
		srv := e.server
		if srv == "" {
			srv = w.server
		}
		w.writeEntries(day, []Entry{{Window: "status", Body: fmt.Sprintf("Disconnected from IRC (%s)", srv)}}, now)
		return
	}

	var line []byte
	if e.buf != nil {
		line = (*e.buf)[:e.n]
	}

	hasEcho := false
	if w.hasCap != nil {
		hasEcho = w.hasCap("echo-message")
	}

	// Format first, then Observe. Format chooses windows from the state the
	// line arrived in: a QUIT or NICK fans out to the channels the nick shared
	// before it happened. Observing first would erase that membership and log
	// the event to status while the replay went to the channels, or vice versa.
	server := w.server
	entries := Format(w.tracker, now, line, e.out, hasEcho, server)
	if !e.out {
		w.tracker.Observe(line)
	}
	w.writeEntries(day, entries, now)
}

func (w *Writer) writeEntries(day string, entries []Entry, now time.Time) {
	if w.logDir == "" {
		return
	}
	if day != w.lastDay {
		if w.lastDay != "" {
			w.indexYesterday(w.lastDay)
		}
		w.lastDay = day
	}
	for _, en := range entries {
		window := en.Window
		if window == "" {
			window = "status"
		}
		path := filepath.Join(w.logDir, w.network, window, day+".log")
		w.mu.Lock()
		if w.file == nil || w.fileDay != day || w.filePath != path {
			if w.file != nil {
				_ = w.file.Close()
				w.file = nil
			}
			if err := os.MkdirAll(filepath.Dir(path), 0755); err == nil {
				f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				if err == nil {
					w.file = f
					w.fileDay = day
					w.filePath = path
				}
			}
		}
		if w.file != nil {
			ts := now.Format("15:04:05")
			_, _ = fmt.Fprintf(w.file, "[%s] %s\n", ts, sanitizeBody(en.Body))
		}
		w.mu.Unlock()
	}
}

func (w *Writer) indexYesterday(day string) {
	if w.logDir == "" {
		return
	}
	w.indexDay(day)
}

// startIndexPass runs the catch-up index pass in the background so neither the
// constructor nor SetNick blocks on file IO. The pass is idempotent: an index
// newer than its log file is skipped.
func (w *Writer) startIndexPass() {
	if w.logDir == "" {
		return
	}
	w.bgMu.Lock()
	if w.closed {
		w.bgMu.Unlock()
		return
	}
	w.wg.Add(1)
	w.bgMu.Unlock()
	go func() {
		defer w.wg.Done()
		w.indexExisting()
	}()
}

// indexExisting builds any missing (or stale) daily indexes for log files that
// existed before this writer started. It never indexes the current day: that
// file is incomplete and is indexed lazily on query instead. An up-to-date
// index is left untouched, so calling this repeatedly is cheap and safe.
func (w *Writer) indexExisting() {
	if w.logDir == "" {
		return
	}
	w.indexMu.Lock()
	defer w.indexMu.Unlock()

	netDir := filepath.Join(w.logDir, w.network)
	windows, err := os.ReadDir(netDir)
	if err != nil {
		return
	}
	today := w.clock().Format("2006-01-02")
	for _, win := range windows {
		if !win.IsDir() {
			continue
		}
		window := win.Name()
		dir := filepath.Join(netDir, window)
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".log") {
				continue
			}
			day := strings.TrimSuffix(f.Name(), ".log")
			if _, err := time.Parse("2006-01-02", day); err != nil {
				continue
			}
			if !dayBefore(day, today) {
				continue
			}
			logPath := filepath.Join(dir, f.Name())
			idxPath := logPath + ".idx"
			if fi, err := os.Stat(logPath); err == nil {
				if ii, err := os.Stat(idxPath); err == nil && !fi.ModTime().After(ii.ModTime()) {
					continue
				}
				if err := BuildIndexFile(logPath, idxPath); err == nil {
					slog.Info("index built", "network", w.network, "window", window, "day", day, "source_bytes", fi.Size())
				}
			}
		}
	}
}

func (w *Writer) indexDay(day string) {
	w.indexMu.Lock()
	defer w.indexMu.Unlock()
	netDir := filepath.Join(w.logDir, w.network)
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		window := e.Name()
		logPath := filepath.Join(netDir, window, day+".log")
		idxPath := logPath + ".idx"
		fi, err := os.Stat(logPath)
		if err != nil {
			continue
		}
		if ii, err := os.Stat(idxPath); err == nil {
			if !fi.ModTime().After(ii.ModTime()) {
				continue
			}
		}
		_ = BuildIndexFile(logPath, idxPath)
	}
}

func dayBefore(day, today string) bool {
	d, err := time.Parse("2006-01-02", day)
	if err != nil {
		return false
	}
	t, err := time.Parse("2006-01-02", today)
	if err != nil {
		return false
	}
	return d.Before(t)
}

// sanitizeBody strips CR, LF and C0 control bytes (except tab) from a log body.
// Bodies are log content, not protocol: an embedded CR would corrupt the
// line-oriented log file. Raw lines on the wire and in the ring are untouched.
func sanitizeBody(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\r' || c == '\n' {
			continue
		}
		if c < 0x20 && c != '\t' {
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
