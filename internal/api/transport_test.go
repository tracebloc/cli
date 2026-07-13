package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// errRoundTripper fails every request with a fixed error — simulating the most
// common real-world API failure the httptest-server tests can't reach:
// connection refused / DNS failure / reset, i.e. HTTP.Do itself erroring.
type errRoundTripper struct{ err error }

func (e errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }

// TestTransportError pins the HTTP.Do-failure branch of the API client
// (client.go get / bodyRequest, previously untested). A dead transport must
// surface a wrapped error from both verbs and from an exported call built on
// them — never a nil error or a zero-value result read as success.
func TestTransportError(t *testing.T) {
	boom := errors.New("connection refused")
	c := &Client{
		BaseURL: "https://api.example.invalid",
		Token:   "t",
		HTTP:    &http.Client{Transport: errRoundTripper{err: boom}},
	}
	ctx := context.Background()

	t.Run("get wraps the transport error", func(t *testing.T) {
		if _, _, err := c.get(ctx, "/whoami"); err == nil || !errors.Is(err, boom) {
			t.Fatalf("get must wrap the transport error (errors.Is), got %v", err)
		}
	})
	t.Run("bodyRequest surfaces the transport error", func(t *testing.T) {
		if _, _, err := c.bodyRequest(ctx, http.MethodPost, "/x", map[string]string{"a": "b"}); err == nil {
			t.Fatal("bodyRequest must error when the transport is down")
		}
	})
	t.Run("an exported call (WhoAmI) fails on a dead transport", func(t *testing.T) {
		if _, err := c.WhoAmI(ctx); err == nil {
			t.Fatal("WhoAmI must fail when HTTP.Do errors, not read a zero-value identity as success")
		}
	})
}
