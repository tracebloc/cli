package push

import (
	"strings"
	"testing"
)

func TestFamilyNoun_RoundTrip(t *testing.T) {
	for _, fam := range []Family{FamilyImage, FamilyText, FamilyTabular} {
		noun := FamilyNoun(fam)
		if noun == "" {
			t.Errorf("FamilyNoun(%v) = empty", fam)
		}
		if got := FamilyFromNoun(noun); got != fam {
			t.Errorf("round-trip FamilyFromNoun(FamilyNoun(%v)) = %v", fam, got)
		}
	}
	// Unrecognized inputs fall back to the picker default (tabular).
	if FamilyNoun(Family(999)) != "tabular" {
		t.Error("an unknown family → 'tabular' noun")
	}
	if FamilyFromNoun("bogus") != FamilyTabular {
		t.Error("an unknown noun → FamilyTabular")
	}
	// FamilyNouns lists the picker options.
	if len(FamilyNouns()) == 0 {
		t.Error("FamilyNouns must not be empty")
	}
}

func TestSupportedCategoriesList(t *testing.T) {
	if list := SupportedCategoriesList(); !strings.Contains(list, "image_classification") {
		t.Errorf("SupportedCategoriesList should include known ids, got %q", list)
	}
}

func TestTextSidecarDir(t *testing.T) {
	if got := TextSidecarDir("masked_language_modeling"); got != "sequences" {
		t.Errorf("TextSidecarDir(masked_language_modeling) = %q, want sequences", got)
	}
	if got := TextSidecarDir("text_classification"); got == "" {
		t.Error("TextSidecarDir(text_classification) must return a subdir")
	}
}
