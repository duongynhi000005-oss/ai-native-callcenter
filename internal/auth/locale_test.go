// SPDX-License-Identifier: Apache-2.0

package auth

import "testing"

// An account that never chose a UI locale reads as DefaultLocale (#289).
func TestLocaleOfDefaultsToEnglish(t *testing.T) {
	zh, empty := "zh-CN", ""
	for _, tc := range []struct {
		name   string
		stored *string
		want   string
	}{
		{"nothing stored", nil, DefaultLocale},
		{"an empty string", &empty, DefaultLocale},
		{"a chosen locale", &zh, "zh-CN"},
	} {
		if got := localeOf(tc.stored); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if DefaultLocale != "en" {
		t.Errorf("DefaultLocale = %q, want \"en\"", DefaultLocale)
	}
}
