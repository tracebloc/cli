package push

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
)

// scanImageResolutions decodes each file's header (cheap, no full decode) and
// sorts it into three buckets — the shared core of the two ImageResolution
// previews: ValidateImages (the images/ dir) and ValidateMaskResolution (the
// masks/ dir). Keeping ONE decode-and-compare here is what lets the CLI mirror
// the ingestor's TWO ImageResolutionValidator instances for semantic
// segmentation (modalities/validators.py) without the rule drifting between
// them.
//
//   - broken:     zero-byte, unreadable, or undecodable files;
//   - tooSmall:   below the minW×minH floor (EITHER side under), when a floor is
//     set — mirrors _meets_min_size;
//   - mismatched: resolution != expectedW×expectedH (exact, no resize), when an
//     expected size is set.
//
// expectedW/H or minW/H of 0 disables that comparison, exactly as documented on
// ValidateImages (see it for the parity rationale). Each offender string carries
// the file name and its dimensions for the caller's message.
func scanImageResolutions(paths []string, expectedW, expectedH, minW, minH int) (broken, tooSmall, mismatched []string) {
	for _, path := range paths {
		name := filepath.Base(path)
		f, err := os.Open(path)
		if err != nil {
			broken = append(broken, fmt.Sprintf("%s (unreadable: %v)", name, err))
			continue
		}
		cfg, _, err := image.DecodeConfig(f)
		_ = f.Close()
		if err != nil {
			if st, serr := os.Stat(path); serr == nil && st.Size() == 0 {
				broken = append(broken, name+" (empty file, 0 bytes)")
			} else {
				broken = append(broken, name+" (not a valid image — corrupt or unsupported format)")
			}
			continue
		}
		if minW > 0 && minH > 0 && (cfg.Width < minW || cfg.Height < minH) {
			tooSmall = append(tooSmall, fmt.Sprintf("%s (%dx%d)", name, cfg.Width, cfg.Height))
		}
		if expectedW > 0 && expectedH > 0 && (cfg.Width != expectedW || cfg.Height != expectedH) {
			mismatched = append(mismatched, fmt.Sprintf("%s (%dx%d)", name, cfg.Width, cfg.Height))
		}
	}
	return broken, tooSmall, mismatched
}

// ValidateMaskResolution previews the ingestor's SECOND ImageResolutionValidator
// for semantic_segmentation — the one named "Mask Resolution Validator" with
// subdir="masks" (modalities/validators.py semantic_segmentation): it reads
// every PNG mask and rejects zero-byte / undecodable files, masks below the
// minimum-size floor, and any mask whose resolution differs from the expected
// target size. Masks are pixel-wise label maps, so they must share the images'
// resolution — the ingestor constructs this validator with the SAME
// expected_resolution (target_size) and min_size as the images'
// ImageResolutionValidator, and this preview is called with those same values.
//
// Before cli#352 the CLI validated only images/ resolution (ValidateImages) and
// never the masks, so a corrupt or mis-sized mask passed local preflight and
// then failed in-cluster after the full upload — the accept-then-reject the
// parity contract exists to prevent. The too-small floor takes precedence over
// the resolution mismatch, matching ValidateImages and the ingestor's ordering.
func ValidateMaskResolution(masks []string, expectedW, expectedH, minW, minH int) error {
	const maxListed = 5
	broken, tooSmall, mismatched := scanImageResolutions(masks, expectedW, expectedH, minW, minH)
	if len(tooSmall) > 0 {
		return fmt.Errorf(
			"%d mask(s) are smaller than the %dx%d minimum you set with --min-size: %s. "+
				"Provide larger masks, or lower the floor with --min-size, then re-run.",
			len(tooSmall), minW, minH, TruncateList(tooSmall, maxListed))
	}
	if len(broken) > 0 {
		return fmt.Errorf(
			"%d mask(s) in masks/ can't be ingested: %s. The cluster reads every mask as a PNG "+
				"and rejects these after the upload — fix or remove them and re-run.",
			len(broken), TruncateList(broken, maxListed))
	}
	if len(mismatched) > 0 {
		return fmt.Errorf(
			"%d mask(s) don't match the %dx%d resolution the images use: %s. Semantic-segmentation "+
				"masks are pixel-wise label maps, so each mask must be exactly the image size — the "+
				"cluster validates this after the upload. Resize the masks to match and re-run.",
			len(mismatched), expectedW, expectedH, TruncateList(mismatched, maxListed))
	}
	return nil
}
