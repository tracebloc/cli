package api

import (
	"context"
	"net/http"
	"testing"
)

// TestGetClient covers the single-client detail fetch (GET /edge-device/{id}/)
// that backs the home-screen heartbeat (cli#338): a 2xx decodes one client, a
// 404 is (nil, nil) — "no such client", not an error — and any other non-2xx is
// an APIError. It also pins the path so the heartbeat can't regress to a list.
func TestGetClient(t *testing.T) {
	t.Run("2xx decodes a single client + hits the detail path", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/edge-device/1070/" {
				t.Errorf("path = %q, want /edge-device/1070/ (detail route, not the list)", r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"id":1070,"first_name":"asad-macbook","status":1,"namespace":"asad-macbook-3"}`))
		})
		pc, err := c.GetClient(context.Background(), 1070)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pc == nil || pc.ID != 1070 || pc.Status != 1 {
			t.Fatalf("GetClient = %+v, want id=1070 status=1", pc)
		}
	})

	t.Run("404 -> (nil, nil), not an error", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
		pc, err := c.GetClient(context.Background(), 999)
		if pc != nil || err != nil {
			t.Fatalf("404 must be (nil, nil); got (%+v, %v)", pc, err)
		}
	})

	t.Run("non-2xx (500) -> APIError", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"detail":"boom"}`))
		})
		if _, err := c.GetClient(context.Background(), 1); err == nil {
			t.Error("a 500 must return an error")
		}
	})
}
