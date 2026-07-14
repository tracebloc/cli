package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAllEndpoints_TransportError covers the "post/get returned an error" arm of
// every endpoint at once, via a transport that fails every request.
func TestAllEndpoints_TransportError(t *testing.T) {
	c := &Client{
		BaseURL: "https://api.invalid",
		Token:   "t",
		HTTP:    &http.Client{Transport: errRoundTripper{err: errors.New("dead transport")}},
	}
	ctx := context.Background()
	if _, err := c.RequestDeviceCode(ctx); err == nil {
		t.Error("RequestDeviceCode must surface a transport error")
	}
	if _, err := c.PollToken(ctx, "x"); err == nil {
		t.Error("PollToken must surface a transport error")
	}
	if err := c.RevokeToken(ctx); err == nil {
		t.Error("RevokeToken must surface a transport error")
	}
	if _, _, err := c.CreateClient(ctx, CreateClientRequest{}); err == nil {
		t.Error("CreateClient must surface a transport error")
	}
	if _, err := c.PatchClientClusterID(ctx, 1, "c"); err == nil {
		t.Error("PatchClientClusterID must surface a transport error")
	}
	if err := c.RevokeClient(ctx, 1); err == nil {
		t.Error("RevokeClient must surface a transport error")
	}
	if _, err := c.ListClients(ctx); err == nil {
		t.Error("ListClients must surface a transport error")
	}
	if _, err := c.ListClientAdmins(ctx); err == nil {
		t.Error("ListClientAdmins must surface a transport error")
	}
}

func TestRequestDeviceCode_EdgeCases(t *testing.T) {
	t.Run("missing device/user code", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
		if _, err := c.RequestDeviceCode(context.Background()); err == nil {
			t.Error("empty device_code/user_code must error")
		}
	})
	t.Run("bad JSON", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{bad`)) })
		if _, err := c.RequestDeviceCode(context.Background()); err == nil {
			t.Error("bad JSON must error")
		}
	})
}

func TestPollToken_EdgeCases(t *testing.T) {
	t.Run("2xx missing token", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) })
		if _, err := c.PollToken(context.Background(), "x"); err == nil {
			t.Error("a success response with no token must error")
		}
	})
	t.Run("expired_token maps to the sentinel", func(t *testing.T) {
		c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"expired_token"}`))
		})
		if _, err := c.PollToken(context.Background(), "x"); !errors.Is(err, ErrExpiredToken) {
			t.Errorf("PollToken(expired_token) = %v, want ErrExpiredToken", err)
		}
	})
}

func TestCreateAndPatch_BadJSON(t *testing.T) {
	c := stubClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{bad`)) })
	ctx := context.Background()
	if _, _, err := c.CreateClient(ctx, CreateClientRequest{}); err == nil {
		t.Error("CreateClient must error on an undecodable 2xx body")
	}
	if _, err := c.PatchClientClusterID(ctx, 1, "c"); err == nil {
		t.Error("PatchClientClusterID must error on an undecodable 2xx body")
	}
}

// TestListClients_Pagination covers the DRF `next`-following loop + nextPath.
func TestListClients_Pagination(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		if page == 1 {
			_, _ = fmt.Fprintf(w, `{"results":[{"id":1,"first_name":"a"}],"next":%q}`,
				"http://"+r.Host+"/edge-device/?page=2")
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":2,"first_name":"b"}],"next":null}`))
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: "t", HTTP: srv.Client()}
	clients, err := c.ListClients(context.Background())
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(clients) != 2 {
		t.Errorf("expected 2 clients across 2 pages, got %d", len(clients))
	}
}
