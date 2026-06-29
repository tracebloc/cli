package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tracebloc/cli/internal/config"
)

// installLog is an append-only, timestamped record of a connect/provision run,
// always written to ~/.tracebloc/install-<ts>.log regardless of --verbose. The
// connect flow is zero-prompt on a headless box, so a failed run must leave a
// full trace on disk to inspect or send to support even when the terminal stayed
// quiet (RFC-0001 §8.5).
//
// A nil *installLog is a no-op on every method, so callers never have to guard:
// logging must never be what fails a provision.
type installLog struct {
	f *os.File
}

// newInstallLog creates ~/.tracebloc/install-<ts>.log (mode 0600 — it can carry
// hostnames and paths). It returns the log (nil if it couldn't be opened) and
// the path it used, so the caller can surface the path without ever failing the
// command over logging.
func newInstallLog() (*installLog, string) {
	dir, err := config.Dir()
	if err != nil {
		return nil, ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, ""
	}
	path := filepath.Join(dir, "install-"+time.Now().UTC().Format("20060102-150405")+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		// No file was created — return an empty path so the caller never
		// advertises a "Full log:" location that doesn't exist (Bugbot).
		return nil, ""
	}
	l := &installLog{f: f}
	l.Logf("tracebloc connect/provision log")
	return l, path
}

// Logf appends a UTC-timestamped line. Safe on a nil receiver (no-op).
func (l *installLog) Logf(format string, a ...any) {
	if l == nil || l.f == nil {
		return
	}
	_, _ = fmt.Fprintf(l.f, "%s  %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

// Close closes the underlying file. Safe on a nil receiver (no-op).
func (l *installLog) Close() {
	if l == nil || l.f == nil {
		return
	}
	_ = l.f.Close()
}
