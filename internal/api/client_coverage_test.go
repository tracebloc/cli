package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubClient points a Client at a one-handler httptest server.
func stubClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "tok", HTTP: srv.Client()}
}

func TestAPIError_ErrorString(t *testing.T) {
	got := (&APIError{StatusCode: 404, Body: "  not found  ", URL: "http://x/y"}).Error()
	for _, w := range []string{"404", "not found", "http://x/y"} {
		if !strings.Contains(got, w) {
			t.Errorf("APIError.Error() = %q, missing %q", got, w)
		}
	}
}

func TestRevokeClient_Cases(t *testing.T) {
	t.Run("2xx returns nil", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		if err := c.RevokeClient(context.Background(), 7); err != nil {
			t.Errorf("RevokeClient(204) = %v, want nil", err)
		}
	})
	t.Run("non-2xx returns APIError", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		})
		if err := c.RevokeClient(context.Background(), 7); err == nil {
			t.Error("RevokeClient(500) must error")
		}
	})
}

func TestListClientAdmins_Cases(t *testing.T) {
	t.Run("2xx parses the list", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[{"name":"A","email":"a@b.io"}]`))
		})
		admins, err := c.ListClientAdmins(context.Background())
		if err != nil || len(admins) != 1 || admins[0].Email != "a@b.io" {
			t.Errorf("ListClientAdmins = %+v, %v", admins, err)
		}
	})
	t.Run("non-2xx returns APIError", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
		if _, err := c.ListClientAdmins(context.Background()); err == nil {
			t.Error("ListClientAdmins(403) must error")
		}
	})
	t.Run("bad JSON returns a decode error", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{not an array}`)) })
		if _, err := c.ListClientAdmins(context.Background()); err == nil {
			t.Error("ListClientAdmins(bad JSON) must error")
		}
	})
}

// TestClientEndpoints_Non2xxError covers the non-2xx error arm of every remaining
// endpoint in one 500 stub (none of these retry, so no hang risk).
func TestClientEndpoints_Non2xxError(t *testing.T) {
	c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
	})
	ctx := context.Background()
	if _, err := c.RequestDeviceCode(ctx); err == nil {
		t.Error("RequestDeviceCode must error on 500")
	}
	if _, err := c.PollToken(ctx, "code"); err == nil {
		t.Error("PollToken must error on a 500 (non-pending)")
	}
	if err := c.RevokeToken(ctx); err == nil {
		t.Error("RevokeToken must error on 500")
	}
	if _, _, err := c.CreateClient(ctx, CreateClientRequest{}); err == nil {
		t.Error("CreateClient must error on 500")
	}
	if _, err := c.PatchClientClusterID(ctx, 1, "cid"); err == nil {
		t.Error("PatchClientClusterID must error on 500")
	}
	if _, err := c.ListClients(ctx); err == nil {
		t.Error("ListClients must error on 500")
	}
	if _, err := c.WhoAmI(ctx); err == nil {
		t.Error("WhoAmI must error on 500")
	}
}

// TestClientEndpoints_DecodeError covers the 2xx-but-undecodable arms of the
// list/identity endpoints.
func TestClientEndpoints_DecodeError(t *testing.T) {
	c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{{bad`)) })
	ctx := context.Background()
	if _, err := c.WhoAmI(ctx); err == nil {
		t.Error("WhoAmI must error on undecodable 200 body")
	}
	if _, err := c.ListClients(ctx); err == nil {
		t.Error("ListClients must error on undecodable 200 body")
	}
}
