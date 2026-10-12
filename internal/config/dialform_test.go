// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestOutboundDialFormSetting(t *testing.T) {
	for _, tt := range []struct {
		value, want string
		wantErr     bool
	}{
		{"", "E164", false},
		{"E164", "E164", false},
		{"E164_NO_PLUS", "E164_NO_PLUS", false},
		{"NATIONAL", "NATIONAL", false},
		{"national", "", true},
		{"PLUS", "", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("AICC_OUTBOUND_DIAL_FORM", tt.value)
			c, err := Load()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "AICC_OUTBOUND_DIAL_FORM") {
					t.Fatalf("Load() error = %v, want one naming the setting", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if string(c.OutboundDialForm) != tt.want {
				t.Errorf("OutboundDialForm = %s, want %s", c.OutboundDialForm, tt.want)
			}
		})
	}
}
