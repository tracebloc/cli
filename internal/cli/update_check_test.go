package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int // -1 a<b, 0 equal, 1 a>b
	}{
		{"0.9.5", "0.9.6", -1},
		{"0.9.5", "0.9.5", 0},
		{"0.9.6", "0.9.5", 1},
		{"1.0.0", "0.9.9", 1},
		{"0.10.0", "0.9.9", 1}, // numeric, not lexical (10 > 9)
		{"v0.9.5", "0.9.5", 0}, // leading v ignored
		{"0.9.5", "v0.9.6", -1},
		{"0.9.5-rc1", "0.9.5", -1}, // pre-release ranks below the release
		{"0.9.5", "0.9.5-rc1", 1},
		{"1.2.3", "1.2", 1}, // missing patch treated as 0
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
		// versionLess is the strict-less wrapper.
		if got := versionLess(c.a, c.b); got != (c.want < 0) {
			t.Errorf("versionLess(%q,%q) = %v, want %v", c.a, c.b, got, c.want < 0)
		}
	}
}

func TestUpdateCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, updateCacheFile)
	in := updateCache{CheckedAt: time.Now().Truncate(time.Second), Latest: "0.9.7"}
	if err := writeUpdateCache(path, in); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, ok := readUpdateCache(path)
	if !ok || got.Latest != "0.9.7" || !got.CheckedAt.Equal(in.CheckedAt) {
		t.Fatalf("round-trip = %+v ok=%v, want %+v", got, ok, in)
	}
	// A missing / empty-Latest cache reads as not-present.
	if _, ok := readUpdateCache(filepath.Join(dir, "nope.json")); ok {
		t.Error("missing cache should read as absent")
	}
}

func TestFetchLatestRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.9.7","name":"ignored"}`))
	}))
	defer srv.Close()
	got, err := fetchLatestRelease(srv.URL, 2*time.Second)
	if err != nil || got != "0.9.7" {
		t.Fatalf("fetchLatestRelease = %q, %v; want 0.9.7, nil", got, err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer bad.Close()
	if _, err := fetchLatestRelease(bad.URL, 2*time.Second); err == nil {
		t.Error("non-200 should error")
	}
}

func TestLatestReleaseVersion_FreshCacheSkipsNetwork(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	// A server that fails the test if it's ever hit — proves a fresh cache is used.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("network hit despite a fresh cache")
	}))
	defer srv.Close()
	swapURL(t, srv.URL)

	if err := writeUpdateCache(updateCachePath(), updateCache{CheckedAt: time.Now(), Latest: "0.9.9"}); err != nil {
		t.Fatal(err)
	}
	if got := latestReleaseVersion(); got != "0.9.9" {
		t.Errorf("latestReleaseVersion = %q, want 0.9.9 (from fresh cache)", got)
	}
}

func TestLatestReleaseVersion_StaleCacheFetchesAndRewrites(t *testing.T) {
	t.Setenv("TRACEBLOC_CONFIG_DIR", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.0"}`))
	}))
	defer srv.Close()
	swapURL(t, srv.URL)

	// Stale cache (older than the interval) → must fetch.
	if err := writeUpdateCache(updateCachePath(), updateCache{CheckedAt: time.Now().Add(-48 * time.Hour), Latest: "0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if got := latestReleaseVersion(); got != "1.2.0" {
		t.Errorf("latestReleaseVersion = %q, want 1.2.0 (fetched)", got)
	}
	// The fetch should have rewritten the cache with the fresh value + time.
	if c, ok := readUpdateCache(updateCachePath()); !ok || c.Latest != "1.2.0" || time.Since(c.CheckedAt) > time.Minute {
		t.Errorf("cache not refreshed: %+v ok=%v", c, ok)
	}
}

func TestUpdateChecksAllowed_SkipConditions(t *testing.T) {
	var buf bytes.Buffer // non-*os.File → never a terminal
	t.Setenv("CI", "")
	t.Setenv("TRACEBLOC_NO_UPDATE_CHECK", "")

	if updateChecksAllowed("dev", os.Stderr) {
		t.Error("dev build must skip")
	}
	if updateChecksAllowed("0.9.5", &buf) {
		t.Error("non-terminal writer must skip")
	}
	t.Setenv("TRACEBLOC_NO_UPDATE_CHECK", "1")
	if updateChecksAllowed("0.9.5", os.Stderr) {
		t.Error("opt-out must skip")
	}
	t.Setenv("TRACEBLOC_NO_UPDATE_CHECK", "")
	t.Setenv("CI", "true")
	if updateChecksAllowed("0.9.5", os.Stderr) {
		t.Error("CI must skip")
	}
}

// swapURL points the release check at a test server for the duration of a test.
func swapURL(t *testing.T, url string) {
	t.Helper()
	old := latestReleaseURL
	latestReleaseURL = url
	t.Cleanup(func() { latestReleaseURL = old })
}
