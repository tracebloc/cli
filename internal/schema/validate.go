package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"
)

// englishPrinter is the per-package message.Printer we pass to
// jsonschema's ErrorKind.LocalizedString. The library panics on a
// nil printer (it tries to construct one with a nil language tag);
// having a process-wide English printer avoids that and keeps the
// output language stable across customer environments. Internationalizing
// the validator output is a v2 concern.
var englishPrinter = message.NewPrinter(language.English)

// V1SchemaID is the $id of the embedded v1 schema. Used as the
// "source URL" when compiling — jsonschema/v6 needs a name to
// associate the loaded bytes with, and the canonical $id keeps
// error messages anchored to the same identifier the Python
// implementation uses.
const V1SchemaID = "https://tracebloc.io/schemas/ingest.v1.json"

// ValidationError is a single schema violation, normalized to the
// "<json-pointer-or-<root>>: <message>" shape that
// tracebloc_ingestor.cli.run._format_errors emits in Python. Pinning
// the format lets us tell customers "the CLI and the server-side
// validator say the same thing about your YAML" with high
// confidence.
type ValidationError struct {
	// Path is the json-pointer-style location, e.g. "spec.processors.0.script".
	// "<root>" for top-level violations (additionalProperties,
	// missing required fields at the document level, etc.).
	Path string

	// Message is the human-readable description of the violation,
	// taken directly from the underlying jsonschema/v6 library's
	// per-error message. The wording is stable across the library's
	// minor versions; we don't post-process it.
	Message string
}

// Format returns the canonical "  <path>: <message>" line, matching
// the indentation the Python implementation produces. The leading
// two spaces are deliberate — they match _format_errors so customer
// docs / runbooks can reference one wording across both
// implementations.
func (e ValidationError) Format() string {
	return fmt.Sprintf("  %s: %s", e.Path, e.Message)
}

// FormatErrors renders a slice of violations as one error per line,
// deterministically ordered by Path then Message. Mirrors
// _format_errors.
//
// Pure: the input slice's order is preserved. An earlier version
// called sort.Slice directly on errs, which silently reordered the
// caller's underlying array — bugbot caught this as a hidden side
// effect that contradicted the function's name + doc. Copying the
// slice header before sorting keeps FormatErrors a true formatter.
func FormatErrors(errs []ValidationError) string {
	// slices.Clone would be cleaner but it's Go 1.21+; the manual
	// copy is portable and the allocation is tiny relative to the
	// schema validation itself.
	sorted := make([]ValidationError, len(errs))
	copy(sorted, errs)

	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Path != sorted[j].Path {
			return sorted[i].Path < sorted[j].Path
		}
		return sorted[i].Message < sorted[j].Message
	})

	var b strings.Builder
	for i, e := range sorted {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.Format())
	}
	return b.String()
}

// Validator wraps a compiled jsonschema.Schema for repeated use.
// Compile once at process startup; validate many. Schema compilation
// is non-trivial (the v1 schema has nested if/then/oneOf chains);
// reusing the compiled form is meaningfully faster for batch
// validation.
type Validator struct {
	schema *jsonschema.Schema
}

// NewV1Validator compiles the embedded v1 schema. Returns an error
// only if the embedded JSON is malformed — which can only happen if
// scripts/sync-schema.sh wrote garbage, in which case the build's
// embed step or the CI drift check would have caught it. The error
// path stays for defense-in-depth.
func NewV1Validator() (*Validator, error) {
	var raw any
	if err := json.Unmarshal(V1Bytes, &raw); err != nil {
		return nil, fmt.Errorf("embedded schema is not valid JSON: %w", err)
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(V1SchemaID, raw); err != nil {
		return nil, fmt.Errorf("registering embedded schema: %w", err)
	}
	s, err := c.Compile(V1SchemaID)
	if err != nil {
		return nil, fmt.Errorf("compiling embedded schema: %w", err)
	}
	return &Validator{schema: s}, nil
}

// ValidateYAML parses the input as YAML and validates the resulting
// document against the schema. Returns the parsed document (useful
// for callers that want to inspect category/table/etc. after a
// successful validation) plus the list of violations.
//
// Two failure modes are distinct and important to separate:
//
//   - The input isn't valid YAML at all (parseErr != nil): callers
//     should surface this as a parse-level error, separately from
//     schema violations.
//   - The input parses but doesn't match the schema (errs non-empty):
//     these are the customer-facing "your config has problems" cases.
func (v *Validator) ValidateYAML(input []byte) (parsed map[string]any, errs []ValidationError, parseErr error) {
	if len(bytes.TrimSpace(input)) == 0 {
		return nil, nil, fmt.Errorf("input is empty")
	}

	var doc any
	if err := yaml.Unmarshal(input, &doc); err != nil {
		return nil, nil, fmt.Errorf("not valid YAML: %w", err)
	}

	// The schema expects a mapping at the top. Anything else (a
	// sequence, a scalar) gets surfaced as a parse-level error so
	// the customer doesn't see a wall of unhelpful schema messages
	// blaming the document's type.
	asMap, ok := doc.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf(
			"document must be a YAML mapping at the top level (apiVersion / kind / category / ...); got %T",
			doc,
		)
	}

	// Schema validators expect canonical Go-native types
	// (map[string]any, []any, string, float64, bool, nil). yaml.v3
	// produces those directly from Unmarshal-into-any, so no
	// conversion needed.
	if err := v.schema.Validate(doc); err != nil {
		// Recurse the error tree into our flat ValidationError list.
		// jsonschema/v6 returns a *ValidationError tree where each
		// node may have child Causes — we flatten to leaves so the
		// customer sees one line per actual problem, not an outline
		// of the validator's traversal path.
		var ve *jsonschema.ValidationError
		if errors_as(err, &ve) {
			errs = flattenValidationError(ve)
		} else {
			// Defensive: shouldn't happen with current jsonschema/v6,
			// but fall back to the raw error string rather than
			// crashing.
			errs = []ValidationError{{Path: "<root>", Message: err.Error()}}
		}
	}

	return asMap, errs, nil
}

// flattenValidationError walks the jsonschema error tree to produce
// one leaf error per real violation. The library returns a tree
// because oneOf / anyOf / allOf can fail in multiple ways at once;
// we want each leaf surfaced individually so the customer can see
// every problem with a single validate run.
func flattenValidationError(ve *jsonschema.ValidationError) []ValidationError {
	if ve == nil {
		return nil
	}

	// Internal nodes (with causes) carry the structural context;
	// the actual violations live at the leaves.
	if len(ve.Causes) > 0 {
		var out []ValidationError
		for _, c := range ve.Causes {
			out = append(out, flattenValidationError(c)...)
		}
		return out
	}

	return []ValidationError{{
		Path:    instanceLocationToPath(ve.InstanceLocation),
		Message: ve.ErrorKind.LocalizedString(englishPrinter),
	}}
}

// instanceLocationToPath converts jsonschema's slice-of-strings
// instance pointer into the dotted form data-ingestors uses
// (matching _format_errors's `".".join(...)`). An empty location
// becomes "<root>" so the customer sees something concrete instead
// of a blank-looking error line.
func instanceLocationToPath(loc []string) string {
	if len(loc) == 0 {
		return "<root>"
	}
	return strings.Join(loc, ".")
}

// errors_as is a tiny inlining of errors.As to avoid the import
// just for this one site. Keeps the dependency graph of this file
// minimal; the only third-party imports stay jsonschema + yaml.
func errors_as(err error, target **jsonschema.ValidationError) bool {
	for cur := err; cur != nil; cur = unwrap(cur) {
		if ve, ok := cur.(*jsonschema.ValidationError); ok {
			*target = ve
			return true
		}
	}
	return false
}

func unwrap(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}
