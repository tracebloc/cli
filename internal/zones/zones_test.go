package zones

import "testing"

func TestValidAndName(t *testing.T) {
	if !Valid("DE") || Name("DE") != "Germany" {
		t.Errorf("DE: valid=%v name=%q", Valid("DE"), Name("DE"))
	}
	if !Valid("US-CAL-CISO") {
		t.Error("US-CAL-CISO should be valid (a sub-zone)")
	}
	if Valid("de") {
		t.Error("lower-case 'de' must be invalid — codes are upper-case")
	}
	if Valid("Germany") || Valid("") || Valid("eu-central-1") {
		t.Error("a name / empty / cloud-region must be invalid")
	}
	if Count() < 100 {
		t.Errorf("expected the full zone list, got %d", Count())
	}
}

func TestSuggest(t *testing.T) {
	has := func(s []string, want string) bool {
		for _, v := range s {
			if v == want {
				return true
			}
		}
		return false
	}
	if got := Suggest("germany", 3); !has(got, "DE") {
		t.Errorf("Suggest(germany) = %v, want it to include DE (name match)", got)
	}
	if got := Suggest("de", 3); !has(got, "DE") {
		t.Errorf("Suggest(de) = %v, want DE (wrong case)", got)
	}
	if got := Suggest("US", 5); !has(got, "US") {
		t.Errorf("Suggest(US) = %v, want US (prefix)", got)
	}
	if got := Suggest("zzzznotazone", 3); len(got) != 0 {
		t.Errorf("Suggest(garbage) = %v, want none", got)
	}
	if got := Suggest("DE", 0); got != nil {
		t.Errorf("Suggest with n=0 = %v, want nil", got)
	}
}
