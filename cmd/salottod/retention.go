package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/iakat/salotto/internal/logfile"
)

// retentionSweepInterval is deliberately coarse. A day file changes age at
// midnight, not continuously, so sweeping more often would only make the
// process touch the log tree for no new decision. Six hours also means a
// restart's startup sweep and the first ticker sweep do not overlap.
const retentionSweepInterval = 6 * time.Hour

// retentionStatus is the last sweep, kept behind a mutex so /stats can read it
// while the ticker writes it. The alternative — a ticker inside
// internal/logfile — would make the writer package start goroutines behind its
// caller's back, which is both untestable without them and impossible to shut
// down with the rest of the daemon. The caller owns the clock here.
type retentionStatus struct {
	mu   sync.Mutex
	days int
	last logfile.SweepResult
	at   time.Time
	err  error
}

func newRetentionStatus(days int) *retentionStatus {
	return &retentionStatus{days: days}
}

// sweep runs one pass and logs what it actually removed. One line at info per
// sweep is enough for an operator to answer "is retention running" from the
// daemon log without turning every six hours into a paragraph.
func (s *retentionStatus) sweep(logDir string, logger *slog.Logger) {
	now := time.Now()
	res, err := logfile.Sweep(logDir, time.Duration(s.days)*24*time.Hour, now)
	s.mu.Lock()
	s.last = res
	s.at = now
	s.err = err
	s.mu.Unlock()

	args := []any{
		"retention_days", s.days,
		"files", res.Files,
		"bytes", res.Bytes,
		"indexes", res.Indexes,
		"windows", res.Windows,
	}
	if err != nil {
		args = append(args, "err", err)
	}
	logger.Info("log retention sweep", args...)
}

// loop owns the ticker for as long as ctx lives. It does not sweep: the startup
// sweep already ran before this goroutine started, so the first tick six hours
// later is the earliest second pass.
func (s *retentionStatus) loop(ctx context.Context, logDir string, logger *slog.Logger) {
	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(logDir, logger)
		}
	}
}

// writeStats adds the retention block to the existing /stats text response. A
// separate endpoint would be a second place to look for the same answer; the
// stats listener already publishes one key=value line per fact.
func (s *retentionStatus) writeStats(b *strings.Builder) {
	s.mu.Lock()
	defer s.mu.Unlock()

	lastUnix := int64(0)
	if !s.at.IsZero() {
		lastUnix = s.at.Unix()
	}
	fmt.Fprintf(b, "log.retention_days=%d\n", s.days)
	fmt.Fprintf(b, "log.retention.last_sweep_unix=%d\n", lastUnix)
	fmt.Fprintf(b, "log.retention.files_deleted=%d\n", s.last.Files)
	fmt.Fprintf(b, "log.retention.bytes_deleted=%d\n", s.last.Bytes)
	fmt.Fprintf(b, "log.retention.indexes_deleted=%d\n", s.last.Indexes)
	fmt.Fprintf(b, "log.retention.windows_removed=%d\n", s.last.Windows)
	if s.err != nil {
		fmt.Fprintf(b, "log.retention.error=%q\n", s.err.Error())
	}
}
