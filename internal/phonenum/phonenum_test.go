// SPDX-License-Identifier: Apache-2.0

package phonenum

import "testing"

func TestCanonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, cc, want string
		reason             Reason
	}{
		{"already canonical", "+8613800138000", "86", "+8613800138000", ""},
		{"national with trunk zero", "013800138000", "86", "+8613800138000", ""},
		{"national without trunk zero", "13800138000", "86", "+8613800138000", ""},
		{"spaced national", "138 0013 8000", "86", "+8613800138000", ""},
		{"spaced plus", "+86 138 0013 8000", "86", "+8613800138000", ""},
		{"us punctuation", "+1 (415) 555-0100", "86", "+14155550100", ""},
		{"dots", "+44.20.7946.0958", "86", "+442079460958", ""},
		{"double zero", "008613800138000", "1", "+8613800138000", ""},
		{"double zero with separators", "00 86-138-0013-8000", "1", "+8613800138000", ""},
		{"empty", "", "86", "", NumberEmpty},
		{"separators only", " - ", "86", "", NumberEmpty},
		{"plus alone", "+", "86", "", NumberInvalid},
		{"double plus", "++8613800138000", "86", "", NumberInvalid},
		{"plus inside", "86+13800138000", "86", "", NumberInvalid},
		{"zero after plus", "+0123456789", "86", "", NumberInvalid},
		{"too short", "+1234567", "86", "", NumberInvalid},
		{"shortest", "+12345678", "86", "+12345678", ""},
		{"longest", "+123456789012345", "86", "+123456789012345", ""},
		{"too long", "+1234567890123456", "86", "", NumberInvalid},
		{"letters", "+86138abc", "86", "", NumberInvalid},
		{"trailing newline", "+8613800138000\n", "86", "", NumberInvalid},
		{"full-width plus", "＋8613800138000", "86", "", NumberInvalid},
		{"arabic-indic digits", "+٨٦١٣٨٠٠١٣٨٠٠٠", "86", "", NumberInvalid},
		{"national needs a country code", "13800138000", "", "", NumberInvalid},
		{"an extension is too short", "1002", "86", "", NumberInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Canonical(tt.in, tt.cc)
			if tt.reason != "" {
				if ReasonOf(err) != tt.reason {
					t.Fatalf("Canonical(%q) = %q, %v; want reason %s", tt.in, got, err, tt.reason)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Canonical(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
			if !IsCanonical(got) {
				t.Errorf("result %q is not canonical", got)
			}
		})
	}
}

func TestReasonOfIgnoresOtherErrors(t *testing.T) {
	t.Parallel()
	if got := ReasonOf(nil); got != "" {
		t.Errorf("ReasonOf(nil) = %q", got)
	}
}

func TestDialString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in string
		form     DialForm
		cc       string
		want     string
		applied  bool
	}{
		{"e164 mobile", "+8618688886666", FormE164, "86", "+8618688886666", true},
		{"unknown form is e164", "+8618688886666", "BOGUS", "86", "+8618688886666", true},
		{"no plus mobile", "+8618688886666", FormE164NoPlus, "86", "8618688886666", true},
		{"national mobile", "+8618688886666", FormNational, "86", "18688886666", true},
		{"national landline loses no digit but gains no 0", "+862166668888", FormNational, "86", "2166668888", true},
		{"other country falls back", "+14155550123", FormNational, "86", "+14155550123", false},
		{"other country matches its own code", "+14155550123", FormNational, "1", "4155550123", true},
		{"national without a country code", "+8618688886666", FormNational, "", "+8618688886666", false},
		{"no plus other country", "+14155550123", FormE164NoPlus, "86", "14155550123", true},
		{"invalid: no plus", "8618688886666", FormNational, "86", "8618688886666", false},
		{"invalid: short", "+1234", FormE164NoPlus, "1", "+1234", false},
		{"invalid: letters", "+86186abc86666", FormNational, "86", "+86186abc86666", false},
		{"invalid: empty", "", FormNational, "86", "", false},
		{"invalid: leading zero", "+0123456789", FormNational, "86", "+0123456789", false},
	}
	for _, c := range cases {
		got, ok := DialString(c.in, c.form, c.cc)
		if got != c.want || ok != c.applied {
			t.Errorf("%s: DialString(%q,%s,%q) = %q,%v want %q,%v", c.name, c.in, c.form, c.cc, got, ok, c.want, c.applied)
		}
	}
}

func TestParseDialForm(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]DialForm{"": FormE164, "E164": FormE164, "E164_NO_PLUS": FormE164NoPlus, "NATIONAL": FormNational} {
		if got, err := ParseDialForm(in); err != nil || got != want {
			t.Errorf("ParseDialForm(%q) = %q,%v", in, got, err)
		}
	}
	for _, in := range []string{"national", "E.164", "PLUS"} {
		if _, err := ParseDialForm(in); err == nil {
			t.Errorf("ParseDialForm(%q) accepted", in)
		}
	}
}
