package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/tracebloc/cli/internal/config"
	"github.com/tracebloc/cli/internal/ui"
)

// Update-check: a quiet, throttled nudge so a customer who ran the installer
// once doesn't silently sit on an old CLI forever (backlog F1). We check the
// latest GitHub release at most once per updateCheckInterval, cache the result
// next to the config, and — only when a newer release exists — print one dim
// line pointing at `tracebloc upgrade`. It is strictly best-effort: it never
// blocks meaningfully (the network call is capped at updateCheckTimeout), never
// errors out, and stays silent on a dev build, off a terminal, in CI, or when
// TRACEBLOC_NO_UPDATE_CHECK is set. Applying the update is a separate, explicit
// step (`tracebloc upgrade`); we never touch the binary here.
var (
	updateCheckInterval = 24 * time.Hour
	updateCheckTimeout  = 2 * time.Second
	// var (not const) so tests can point it at an httptest server.
	latestReleaseURL = "https://api.github.com/repos/tracebloc/cli/releases/latest"
)

const updateCacheFile = "update-check.json"

// updateCache is the throttle state: when we last asked GitHub and what it said,
// so we hit the network at most once per updateCheckInterval.
type updateCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"` // normalized, e.g. "0.9.7" (no leading v)
}

// MaybeNotifyUpdate prints a one-line "newer version available" nudge to w when
// the running CLI is behind the latest release. Called once from main after the
// command runs. Safe to call unconditionally — it gates itself.
func MaybeNotifyUpdate(version string, w io.Writer) {
	if !updateChecksAllowed(version, w) {
		return
	}
	latest := latestReleaseVersion()
	if latest == "" || !versionLess(version, latest) {
		return
	}
	p := ui.New(w)
	p.Newline()
	p.Infof("A newer tracebloc is available: %s (you have %s). Update: tracebloc upgrade", latest, version)
}

// updateChecksAllowed gates the nudge: real release build, an interactive
// terminal on w, not CI, not opted out.
func updateChecksAllowed(version string, w io.Writer) bool {
	if os.Getenv("TRACEBLOC_NO_UPDATE_CHECK") != "" || os.Getenv("CI") != "" {
		return false
	}
	if version == "" || version == "dev" || strings.HasPrefix(version, "dev") {
		return false // not a release build — nothing meaningful to compare
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// latestReleaseVersion returns the latest release version (normalized), using
// the on-disk cache when it's fresh and only hitting the network otherwise. On a
// network failure it falls back to any cached value, else "".
func latestReleaseVersion() string {
	path := updateCachePath()
	if c, ok := readUpdateCache(path); ok && time.Since(c.CheckedAt) < updateCheckInterval {
		return c.Latest
	}
	latest, err := fetchLatestRelease(latestReleaseURL, updateCheckTimeout)
	if err != nil {
		if c, ok := readUpdateCache(path); ok {
			// Stale, but better than nothing. Re-stamp CheckedAt to now so the
			// once-per-interval throttle actually holds — otherwise the cache
			// stays expired and every command re-hits the network (and eats the
			// full updateCheckTimeout) while offline or rate-limited.
			_ = writeUpdateCache(path, updateCache{CheckedAt: time.Now(), Latest: c.Latest})
			return c.Latest
		}
		return ""
	}
	_ = writeUpdateCache(path, updateCache{CheckedAt: time.Now(), Latest: latest})
	return latest
}

// fetchLatestRelease reads the tag_name of the latest GitHub release, time-boxed.
func fetchLatestRelease(url string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github releases: HTTP %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return "", err
	}
	v := normalizeVersion(body.TagName)
	if v == "" {
		return "", fmt.Errorf("empty tag_name in release response")
	}
	return v, nil
}

func updateCachePath() string {
	dir, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, updateCacheFile)
}

func readUpdateCache(path string) (updateCache, bool) {
	if path == "" {
		return updateCache{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return updateCache{}, false
	}
	var c updateCache
	if json.Unmarshal(raw, &c) != nil || c.Latest == "" {
		return updateCache{}, false
	}
	return c, true
}

func writeUpdateCache(path string, c updateCache) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// versionLess reports whether a is an older version than b. Dotted numeric
// (major.minor.patch); a leading "v" is ignored; a pre-release suffix ("-rc1")
// ranks BELOW the same core release, so a stable user is never nudged onto a
// pre-release of the version they already run.
func versionLess(a, b string) bool { return compareVersions(a, b) < 0 }

func compareVersions(a, b string) int {
	an, ap := splitVersion(a)
	bn, bp := splitVersion(b)
	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "": // a is a release, b a pre-release of the same core → a is newer
		return 1
	case bp == "":
		return -1
	default:
		return strings.Compare(ap, bp)
	}
}

func splitVersion(v string) ([3]int, string) {
	v = normalizeVersion(v)
	pre := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		pre, v = v[i+1:], v[:i]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		out[i], _ = strconv.Atoi(strings.TrimSpace(part))
	}
	return out, pre
}

func normalizeVersion(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }
