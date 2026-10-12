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
