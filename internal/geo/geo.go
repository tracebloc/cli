// Package geo best-effort detects the host's electricityMaps zone (backend
// ZONE_CHOICES) to pre-fill `client create`'s location prompt: cloud instance
// metadata first (high confidence — the VM reports its own region), then IP
// geolocation (low confidence — flagged). The result is only ever a SUGGESTED
// default the user confirms or overrides; detection failing just means an empty
// default (RFC-0001 location auto-detect, cli#84).
package geo

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// Confidence levels for a detected zone.
const (
	High = "high" // cloud instance metadata — the host runs in this region
	Low  = "low"  // IP geolocation — can be wrong behind VPN / proxy / egress NAT
)

// Zone is a best-effort location guess. Code is an ISO 3166-1 alpha-2 country
// (always a valid top-level electricityMaps zone); Source names how it was found.
type Zone struct {
	Code       string
	Source     string
	Confidence string
}

// Metadata / GeoIP endpoints — package vars so tests can point them at httptest.
var (
	awsIMDSBase   = "http://169.254.169.254"
	gcpMetaBase   = "http://metadata.google.internal"
	azureIMDSBase = "http://169.254.169.254"
	geoIPURL      = "https://www.cloudflare.com/cdn-cgi/trace"
)

const (
	cloudProbeTimeout = 1500 * time.Millisecond
	geoIPTimeout      = 3 * time.Second
)

var (
	// Metadata endpoints are link-local — never via a proxy, and fail fast.
	metadataClient = &http.Client{Transport: &http.Transport{Proxy: nil}}
	// GeoIP is a public host — honor the corporate proxy like the API client.
	geoIPClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
)

// Detect returns a best-effort zone, or nil if nothing could be determined
// (offline, egress-restricted, or bare metal with no usable IP geolocation). It
// never blocks long: the cloud probes share one short deadline and run
// concurrently; GeoIP is a single call only reached when the host isn't a
// recognized cloud region.
func Detect(ctx context.Context) *Zone {
	if region, provider := probeCloud(ctx); region != "" {
		if cc, ok := regionCountry(region); ok {
			return &Zone{Code: cc, Source: provider, Confidence: High}
		}
		// A cloud host whose region isn't in the map — fall through to GeoIP for
		// a VALID zone rather than suggest an unknown string the backend rejects.
	}
	if cc := probeGeoIP(ctx); cc != "" {
		return &Zone{Code: cc, Source: "geoip", Confidence: Low}
	}
	return nil
}

// probeCloud runs the three cloud probes concurrently under one deadline and
// returns the first that reports a region (so a real cloud host answers in one
// round-trip instead of waiting through the others' timeouts).
func probeCloud(ctx context.Context) (region, provider string) {
	ctx, cancel := context.WithTimeout(ctx, cloudProbeTimeout)
	defer cancel()
	type res struct{ region, provider string }
	// Snapshot the endpoint bases synchronously, before spawning the goroutines,
	// so each probe reads a captured local — never the package var. We return on
	// the first winner and leave the losers running to their deadline; if they
	// read the globals directly, a test's t.Cleanup (which restores those vars)
	// races the still-running goroutines (go test -race).
	awsBase, gcpBase, azBase := awsIMDSBase, gcpMetaBase, azureIMDSBase
	probes := []struct {
		name string
		fn   func(context.Context) string
	}{
		{"aws", func(c context.Context) string { return detectAWS(c, awsBase) }},
		{"gcp", func(c context.Context) string { return detectGCP(c, gcpBase) }},
		{"azure", func(c context.Context) string { return detectAzure(c, azBase) }},
	}
	ch := make(chan res, len(probes))
	for _, p := range probes {
		p := p
		go func() { ch <- res{p.fn(ctx), p.name} }()
	}
	for range probes {
		if r := <-ch; r.region != "" {
			return r.region, r.provider
		}
	}
	return "", ""
}

// detectAWS reads the region from EC2 IMDS, preferring IMDSv2 (token) and
// falling back to IMDSv1 (no token) if the token PUT is refused.
func detectAWS(ctx context.Context, base string) string {
	var token string
	if req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/latest/api/token", nil); err == nil {
		req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
		if t, ok := doText(metadataClient, req); ok {
			token = t
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/latest/meta-data/placement/region", nil)
	if err != nil {
		return ""
	}
	if token != "" {
		req.Header.Set("X-aws-ec2-metadata-token", token)
	}
	region, _ := doText(metadataClient, req)
	return region
}

// detectGCP reads the instance zone and trims the trailing zone letter to a
// region ("projects/N/zones/europe-west3-c" → "europe-west3").
func detectGCP(ctx context.Context, base string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/computeMetadata/v1/instance/zone", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Metadata-Flavor", "Google")
	zone, ok := doText(metadataClient, req)
	if !ok || zone == "" {
		return ""
	}
	if i := strings.LastIndex(zone, "/"); i >= 0 {
		zone = zone[i+1:]
	}
	if i := strings.LastIndex(zone, "-"); i >= 0 {
		zone = zone[:i]
	}
	return zone
}

// detectAzure reads the compute location from Azure IMDS (already a region-like
// string, e.g. "germanywestcentral").
func detectAzure(ctx context.Context, base string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/metadata/instance/compute/location?api-version=2021-02-01&format=text", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Metadata", "true")
	loc, _ := doText(metadataClient, req)
	return loc
}

// probeGeoIP reads the ISO country from Cloudflare's trace endpoint (the `loc=`
// line) — HTTPS, no API key, returns a 2-letter country code.
func probeGeoIP(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, geoIPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geoIPURL, nil)
	if err != nil {
		return ""
	}
	body, ok := doText(geoIPClient, req)
	if !ok {
		return ""
	}
	for _, line := range strings.Split(body, "\n") {
		if cc, found := strings.CutPrefix(line, "loc="); found {
			cc = strings.TrimSpace(cc)
			if len(cc) == 2 {
				return strings.ToUpper(cc)
			}
		}
	}
	return ""
}

// doText runs req and returns the trimmed body on a 2xx, else ("", false).
func doText(client *http.Client, req *http.Request) (string, bool) {
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}
