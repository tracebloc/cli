package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPProbe pins the real connectivity prober (doctor.go:648, was 0% — the
// checks inject Options.HTTPProbe in tests, so the prober the CLI actually
// ships was never exercised). Any HTTP response = reachable (nil); a dial
// failure = the "down" error; an unbuildable request = error.
func TestHTTPProbe(t *testing.T) {
	t.Run("reachable host (any status) -> nil", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot) // non-2xx still means "connected"
		}))
		defer srv.Close()
		if err := httpProbe(context.Background(), srv.URL); err != nil {
			t.Errorf("a responding host must probe as reachable, got %v", err)
		}
	})
	t.Run("unreachable host -> error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // now refuses connections -> a fast Do error (not the 8s timeout)
		if err := httpProbe(context.Background(), url); err == nil {
			t.Error("a closed host must probe as unreachable")
		}
	})
	t.Run("unbuildable request -> error", func(t *testing.T) {
		if err := httpProbe(context.Background(), "://not a url"); err == nil {
			t.Error("an invalid URL must return the request-build error")
		}
	})
}
