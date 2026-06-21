// Package slug ports the RFC-0001 namespace-slug rule (backend
// common/utils/slug.py, RFC-0001 Appendix B) to Go. A client's display name is
// slugified ONCE at provisioning into the immutable Kubernetes namespace, so
// this MUST stay in lock-step with the Python definition — the backend
// validates exactly what this produces (RFC-0001 backend#830; provisioning +
// namespace validation in backend#836).
package slug

import (
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// MaxLabelLength is the DNS-1123 label cap.
const MaxLabelLength = 63

var (
	nonAlnum  = regexp.MustCompile(`[^a-z0-9]+`)
	multiDash = regexp.MustCompile(`-+`)
)

// Slugify maps name to a DNS-1123 label, or "" if nothing survives. Mirrors
// slug.slugify_dns1123: NFKD-transliterate to ASCII, lowercase, map every run
// of non-alphanumerics to a single "-", trim leading/trailing "-", cap at 63.
func Slugify(name string) string {
	if name == "" {
		return ""
	}
	s := strings.ToLower(toASCII(name))
	s = nonAlnum.ReplaceAllString(s, "-")
	// Redundant after the run-collapse above, but slug.py runs the identical
	// second pass — mirror it so the two stay structurally in lock-step.
	s = multiDash.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > MaxLabelLength {
		s = s[:MaxLabelLength]
	}
	return strings.TrimRight(s, "-")
}

// Derive returns a UNIQUE DNS-1123 slug for name, avoiding taken. On collision
// it appends -2, -3, … within the 63-char cap. If name slugifies to empty it
// falls back to fallback; with an empty fallback it errors (an empty slug must
// never silently become a namespace). Mirrors slug.derive_slug — except Go has
// no nil string, so fallback=="" is treated as "no fallback" (erroring); the
// Python distinguishes None from "" but no caller passes "", and erroring is
// the safer side of that edge.
func Derive(name string, taken []string, fallback string) (string, error) {
	base := Slugify(name)
	if base == "" {
		if fallback == "" {
			return "", fmt.Errorf("name %q slugifies to empty; a fallback is required", name)
		}
		if base = Slugify(fallback); base == "" {
			base = fallback
		}
	}
	set := make(map[string]struct{}, len(taken))
	for _, t := range taken {
		set[t] = struct{}{}
	}
	if _, clash := set[base]; !clash {
		return base, nil
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		end := MaxLabelLength - len(suffix)
		if end > len(base) {
			end = len(base)
		}
		if end < 0 {
			end = 0
		}
		cand := strings.TrimRight(base[:end], "-") + suffix
		if _, clash := set[cand]; !clash {
			return cand, nil
		}
	}
}

func toASCII(s string) string {
	// NFKD decompose then drop non-ASCII — matches Python's
	// unicodedata.normalize("NFKD", s).encode("ascii", "ignore").
	var b strings.Builder
	for _, r := range norm.NFKD.String(s) {
		if r < 128 {
			b.WriteByte(byte(r))
		}
	}
	return b.String()
}
