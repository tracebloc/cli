package geo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// setEndpoints points the metadata / GeoIP endpoints at test servers.
func setEndpoints(t *testing.T, aws, gcp, azure, geoip string) {
	t.Helper()
	oa, og, oz, ogi := awsIMDSBase, gcpMetaBase, azureIMDSBase, geoIPURL
	awsIMDSBase, gcpMetaBase, azureIMDSBase, geoIPURL = aws, gcp, azure, geoip
	t.Cleanup(func() { awsIMDSBase, gcpMetaBase, azureIMDSBase, geoIPURL = oa, og, oz, ogi })
}

// notFoundServer is a stand-in for an absent provider (every probe 404s fast).
func notFoundServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDetect_AWS(t *testing.T) {
	aws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			_, _ = w.Write([]byte("tok-123"))
		case r.Method == http.MethodGet && r.URL.Path == "/latest/meta-data/placement/region":
			if r.Header.Get("X-aws-ec2-metadata-token") != "tok-123" {
				w.WriteHeader(http.StatusUnauthorized) // enforce the IMDSv2 token
				return
			}
			_, _ = w.Write([]byte("eu-central-1"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(aws.Close)
	nf := notFoundServer(t)
	setEndpoints(t, aws.URL, nf, nf, nf)

	if z := Detect(context.Background()); z == nil || z.Code != "DE" || z.Source != "aws" || z.Confidence != High {
		t.Fatalf("got %+v, want DE/aws/high", z)
	}
}

func TestDetect_GCP(t *testing.T) {
	gcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/computeMetadata/v1/instance/zone" && r.Header.Get("Metadata-Flavor") == "Google" {
			_, _ = w.Write([]byte("projects/123/zones/europe-west3-c"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(gcp.Close)
	nf := notFoundServer(t)
	setEndpoints(t, nf, gcp.URL, nf, nf)

	if z := Detect(context.Background()); z == nil || z.Code != "DE" || z.Source != "gcp" || z.Confidence != High {
		t.Fatalf("got %+v, want DE/gcp/high", z)
	}
}

func TestDetect_Azure(t *testing.T) {
	az := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metadata/instance/compute/location" && r.Header.Get("Metadata") == "true" {
			_, _ = w.Write([]byte("germanywestcentral"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(az.Close)
	nf := notFoundServer(t)
	setEndpoints(t, nf, nf, az.URL, nf)

	if z := Detect(context.Background()); z == nil || z.Code != "DE" || z.Source != "azure" || z.Confidence != High {
		t.Fatalf("got %+v, want DE/azure/high", z)
	}
}

func TestDetect_GeoIPFallback(t *testing.T) {
	geoip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fl=1f\nip=1.2.3.4\nloc=FR\ncolo=CDG\n"))
	}))
	t.Cleanup(geoip.Close)
	nf := notFoundServer(t)
	setEndpoints(t, nf, nf, nf, geoip.URL)

	if z := Detect(context.Background()); z == nil || z.Code != "FR" || z.Source != "geoip" || z.Confidence != Low {
		t.Fatalf("got %+v, want FR/geoip/low", z)
	}
}

func TestDetect_UnmappedRegionFallsBackToGeoIP(t *testing.T) {
	// A cloud region we don't map must NOT be suggested verbatim (the backend
	// would reject it) — Detect falls through to GeoIP for a valid zone.
	aws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest/api/token":
			_, _ = w.Write([]byte("t"))
		case "/latest/meta-data/placement/region":
			_, _ = w.Write([]byte("antarctica-south-1"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(aws.Close)
	geoip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("loc=US\n"))
	}))
	t.Cleanup(geoip.Close)
	nf := notFoundServer(t)
	setEndpoints(t, aws.URL, nf, nf, geoip.URL)

	if z := Detect(context.Background()); z == nil || z.Code != "US" || z.Source != "geoip" {
		t.Fatalf("got %+v, want US/geoip (unmapped region → GeoIP)", z)
	}
}

func TestDetect_Nothing(t *testing.T) {
	nf := notFoundServer(t)
	setEndpoints(t, nf, nf, nf, nf)
	if z := Detect(context.Background()); z != nil {
		t.Fatalf("got %+v, want nil", z)
	}
}

func TestRegionCountry(t *testing.T) {
	cases := map[string]string{
		"eu-central-1":       "DE", // AWS
		"europe-west3":       "DE", // GCP
		"germanywestcentral": "DE", // Azure
		"us-east-1":          "US",
		"ap-southeast-1":     "SG",
		"EU-WEST-2":          "GB", // case-insensitive
	}
	for region, want := range cases {
		if got, ok := regionCountry(region); !ok || got != want {
			t.Errorf("regionCountry(%q) = %q,%v; want %q,true", region, got, ok, want)
		}
	}
	if _, ok := regionCountry("mars-north-1"); ok {
		t.Error("unmapped region should return ok=false")
	}
}
