package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SweepResult reports what one sweep actually did. Counts rather than a bare
// error are the point: an operator has to be able to tell "nothing was old
// enough" from "a year of logs was deleted", and a nil error answers neither.
// Bytes covers the day files only; an index is derived data whose size says
// nothing about how much history remains.
type SweepResult struct {
	Files   int
	Bytes   int64
	Indexes int
	Windows int
}

// Sweep deletes day files (and their indexes) older than olderThan, and reports
// what it removed. A zero olderThan keeps everything.
//
// Age comes from the day in the file name, never from the modification time.
// mtime is rewritten by a copy, a restore, a backup and a bind mount, so an
// mtime sweep deletes the wrong day or keeps the wrong one; the name is the
// only authority because it is the one thing the writer set on purpose. A file
// whose name is not exactly YYYY-MM-DD.log is left alone — guessing about an
// unrecognised file in a log directory is how a deleter loses data.
//
// The cutoff is capped at the start of yesterday. A one-day setting must not
// delete the day being written, and a typo such as 0.5 days must not lose the
// current buffer; the last complete day stays whole either way.
//
// Every candidate is cleaned and checked to remain under the cleaned log
// directory before removal. A symlinked day file or window directory, or a
// network name containing ".." from a config file, must not turn a sweep into a
// recursive delete somewhere else. Symlinked entries are skipped rather than
// resolved: resolving is exactly the step that follows a link out of the tree.
//
// The .log is removed before its .idx, so a crash in between leaves an index
// whose log is gone (a miss the query path already ignores) rather than an
// index a search can hit and then fail to open. A window directory that becomes
// empty is removed, and then a network directory that becomes empty, because a
// listing of ten thousand empty channels is what makes a log tree unreadable to
// a human. A sweep that is interrupted is safe to run again: a missing file is
// not an error.
func Sweep(logDir string, olderThan time.Duration, now time.Time) (SweepResult, error) {
	var res SweepResult
	if logDir == "" || olderThan <= 0 {
		return res, nil
	}
	root, err := filepath.Abs(logDir)
	if err != nil {
		return res, err
	}
	root = filepath.Clean(root)

	cutoff := now.Add(-olderThan)
	startOfYesterday := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, now.Location())
	if cutoff.After(startOfYesterday) {
		cutoff = startOfYesterday
	}

	networks, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, err
	}

	var errs []error
	for _, network := range networks {
		// A symlink to a directory reports IsDir false, which is what keeps the
		// walk from following it outside the root.
		if !network.IsDir() {
			continue
		}
		netDir := filepath.Clean(filepath.Join(root, network.Name()))
		if !pathWithin(root, netDir) {
			continue
		}
		windows, err := os.ReadDir(netDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", netDir, err))
			continue
		}
		for _, window := range windows {
			if !window.IsDir() {
				continue
			}
			windowDir := filepath.Clean(filepath.Join(netDir, window.Name()))
			if !pathWithin(root, windowDir) {
				continue
			}
			files, err := os.ReadDir(windowDir)
			if err != nil {
				errs = append(errs, fmt.Errorf("read %s: %w", windowDir, err))
				continue
			}
			for _, file := range files {
				if file.IsDir() || file.Type()&os.ModeSymlink != 0 {
					continue
				}
				day, ok := dayFromLogName(file.Name(), now.Location())
				if !ok || !day.Before(cutoff) {
					continue
				}
				logPath := filepath.Clean(filepath.Join(windowDir, file.Name()))
				if !pathWithin(root, logPath) {
					continue
				}
				info, err := file.Info()
				if err != nil {
					errs = append(errs, fmt.Errorf("stat %s: %w", logPath, err))
					continue
				}
				// Delete the log first and the index second. The order is the
				// crash story: an index whose log is gone is a miss the query
				// path ignores, while a log whose index was removed first would
				// have to be re-indexed from a file we are about to delete.
				if err := os.Remove(logPath); err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						continue
					}
					errs = append(errs, fmt.Errorf("remove %s: %w", logPath, err))
					continue
				}
				res.Files++
				res.Bytes += info.Size()

				idxPath := filepath.Clean(logPath + ".idx")
				if !pathWithin(root, idxPath) {
					continue
				}
				if err := os.Remove(idxPath); err != nil {
					if !errors.Is(err, fs.ErrNotExist) {
						errs = append(errs, fmt.Errorf("remove %s: %w", idxPath, err))
					}
					continue
				}
				res.Indexes++
			}
			removed, err := pruneEmptyDir(windowDir)
			if err != nil {
				errs = append(errs, err)
			} else if removed {
				res.Windows++
			}
		}
		// A network directory is not a window and is not counted, but leaving a
		// skeleton of empty network directories has the same readability cost.
		if _, err := pruneEmptyDir(netDir); err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// dayFromLogName parses exactly YYYY-MM-DD.log in the writer's location. The
// round trip rejects spellings time.Parse would otherwise tolerate, because a
// name this function does not fully understand must not become a delete.
func dayFromLogName(name string, loc *time.Location) (time.Time, bool) {
	if !strings.HasSuffix(name, ".log") {
		return time.Time{}, false
	}
	base := strings.TrimSuffix(name, ".log")
	day, err := time.ParseInLocation("2006-01-02", base, loc)
	if err != nil || day.Format("2006-01-02") != base {
		return time.Time{}, false
	}
	return day, true
}

// pathWithin reports whether candidate is inside root after both are cleaned.
// filepath.Rel is used instead of a string prefix because a prefix accepts
// "/logs-old" for a root of "/logs", which is one separator away from deleting
// outside the tree.
func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// pruneEmptyDir removes dir if it is empty. A directory that is not empty is
// not an error: another writer or a leftover file is a reason to leave it, not
// to fail the sweep. A concurrent writer can create a file between the read and
// the remove, so ENOTEMPTY is treated as benign too.
func pruneEmptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return false, nil
	}
	if err := os.Remove(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) ||
			errors.Is(err, syscall.ENOTEMPTY) ||
			errors.Is(err, syscall.EEXIST) {
			return false, nil
		}
		return false, fmt.Errorf("remove %s: %w", dir, err)
	}
	return true, nil
}
