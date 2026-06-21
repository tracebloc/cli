// Package zones is the CLI's vendored copy of the backend's location zone list
// (ZONE_CHOICES — electricityMaps zones), embedded from zones.json so `client
// create` can validate a location against exactly what the backend's ChoiceField
// accepts, instead of letting an invalid zone fail as a 400 at create time.
// Regenerate with scripts/sync-zones.sh when the backend list changes.
package zones

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
)

//go:embed zones.json
var zonesJSON []byte

// names maps a zone code (e.g. "DE", "US-CAL-CISO") to its display name.
var names map[string]string

func init() {
	if err := json.Unmarshal(zonesJSON, &names); err != nil {
		panic("zones: invalid embedded zones.json: " + err.Error())
	}
}

// Valid reports whether code is a known zone. Case-sensitive: zone codes are
// upper-case, matching the backend ZONE_CHOICES the API validates against.
func Valid(code string) bool {
	_, ok := names[strings.TrimSpace(code)]
	return ok
}

// Name returns the display name for a zone code, or "" if unknown.
func Name(code string) string { return names[strings.TrimSpace(code)] }

// Count returns how many zones are known.
func Count() int { return len(names) }

// Suggest returns up to n plausible zone codes for a (likely invalid) input, to
// help a user who typed "germany", "de", or "Germany" find "DE". Match priority:
// the input as an exact code in the wrong case, then a code prefix, then a
// case-insensitive name substring. De-duplicated; sorted within each tier.
func Suggest(input string, n int) []string {
	q := strings.TrimSpace(input)
	if q == "" || n <= 0 {
		return nil
	}
	upper := strings.ToUpper(q)
	lower := strings.ToLower(q)

	var out []string
	seen := map[string]bool{}
	add := func(code string) bool {
		if !seen[code] {
			seen[code] = true
			out = append(out, code)
		}
		return len(out) >= n
	}

	// 1. exact code, wrong case ("de" → "DE")
	for code := range names {
		if strings.EqualFold(code, q) && add(code) {
			return out
		}
	}
	// 2. code prefix ("US" → US, US-CAL-CISO, …)
	var prefixed []string
	for code := range names {
		if !seen[code] && strings.HasPrefix(code, upper) {
			prefixed = append(prefixed, code)
		}
	}
	sort.Strings(prefixed)
	for _, code := range prefixed {
		if add(code) {
			return out
		}
	}
	// 3. name substring ("german" → DE)
	var named []string
	for code, name := range names {
		if !seen[code] && strings.Contains(strings.ToLower(name), lower) {
			named = append(named, code)
		}
	}
	sort.Strings(named)
	for _, code := range named {
		if add(code) {
			return out
		}
	}
	return out
}
