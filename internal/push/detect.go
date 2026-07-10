package push

import (
	"fmt"
	"image"
	"os"
	"strconv"
	"strings"

	// Register the stdlib image decoders so image.DecodeConfig can
	// read the headers of the formats the image_classification layout
	// accepts (.jpg/.jpeg both decode via image/jpeg). Formats without
	// a registered decoder never reach this function anymore — Discover
	// skips anything outside the ingestor's accept-set (cli#68).
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// DetectImageSize returns the pixel width and height of the image at
// path by decoding only its header (image.DecodeConfig — it does not
// read the pixel data, so it's cheap even for large images).
//
// Supports the stdlib-registered formats (jpeg, png, gif). Returns an
// error for formats without a registered decoder; the caller treats
// that as "couldn't auto-detect" and falls back to the ingestor
// default, advising --target-size. (Since Discover only yields the
// ingestor's accept-set — .jpg/.jpeg/.png — that path is defensive.)
func DetectImageSize(path string) (width, height int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = f.Close() }()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("decoding image header %q: %w", path, err)
	}
	return cfg.Width, cfg.Height, nil
}

// ParseTargetSize parses a --target-size flag value into [width,
// height]. Accepts "WxH" (the documented form, e.g. "512x512") and
// "W,H" as a convenience. Both dimensions must be positive integers.
func ParseTargetSize(s string) (width, height int, err error) {
	return parseWxH("target size", s)
}

// ParseMinSize parses a --min-size flag value into [width, height],
// the same WxH grammar as --target-size (#183). It plumbs to
// spec.file_options.min_size, the ingestor's minimum-image-size floor
// override (data-ingestors #348).
func ParseMinSize(s string) (width, height int, err error) {
	return parseWxH("min size", s)
}

// parseWxH parses a "WxH" (or "W,H") dimension pair, using kind in
// error messages so callers surface "target size …" / "min size …"
// verbatim. Both dimensions must be positive integers.
func parseWxH(kind, s string) (width, height int, err error) {
	sep := "x"
	if strings.Contains(s, ",") {
		sep = ","
	}
	parts := strings.Split(s, sep)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf(
			"%s %q must be WxH (e.g. 512x512)", kind, s)
	}
	width, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf(
			"%s %q: width is not an integer: %w", kind, s, err)
	}
	height, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf(
			"%s %q: height is not an integer: %w", kind, s, err)
	}
	if width <= 0 || height <= 0 {
		return 0, 0, fmt.Errorf(
			"%s %q: width and height must both be positive", kind, s)
	}
	return width, height, nil
}
