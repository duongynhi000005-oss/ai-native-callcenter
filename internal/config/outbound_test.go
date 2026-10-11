// SPDX-License-Identifier: Apache-2.0

package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseOutboundSIPHeaders(t *testing.T) {
	t.Parallel()
	h := func(kv ...string) []SIPHeader {
		var out []SIPHeader
		for i := 0; i < len(kv); i += 2 {
			out = append(out, SIPHeader{kv[i], kv[i+1]})
		}
		return out
	}
	tests := []struct {
		name    string
		raw     string
		want    []SIPHeader
		wantErr string // substring naming the bad pair; empty means success
	}{
		{name: "empty", raw: "", want: nil},
		{name: "blank", raw: "   ", want: nil},
		{name: "single pair", raw: "X-FSBC-Out=1", want: h("X-FSBC-Out", "1")},
		{
			name: "three padded pairs keep their order",
			raw:  "  X-FSBC-Out = 1 ;X-Site=bj ;  X-Z =  last  ",
			want: h("X-FSBC-Out", "1", "X-Site", "bj", "X-Z", "last"),
		},
		{name: "comma, space and quote in a value", raw: "X-Note=a,b c'd", want: h("X-Note", "a,b c'd")},
		{name: "value may hold =", raw: "X-A=b=c", want: h("X-A", "b=c")},
		{name: "2 KB value", raw: "X-Big=" + strings.Repeat("v", 2048), want: h("X-Big", strings.Repeat("v", 2048))},
		{name: "trailing separator", raw: "X-A=1;", wantErr: "empty segment"},
		{name: "leading separator", raw: ";X-A=1", wantErr: "empty segment"},
		{name: "doubled separator", raw: "X-A=1;;X-B=2", wantErr: "empty segment"},
		{name: "only a separator", raw: ";;", wantErr: "empty segment"},
		{name: "missing =", raw: "X-A", wantErr: `pair "X-A" is not Name=value`},
		{name: "empty value", raw: "X-FSBC-Out=", wantErr: `pair "X-FSBC-Out="`},
		{name: "no X- prefix", raw: "Foo=1", wantErr: `pair "Foo=1"`},
		{name: "application namespace", raw: "x-aicc-call-id=1", wantErr: `pair "x-aicc-call-id=1"`},
		{name: "duplicate differing in case", raw: "X-A=1;x-a=2", wantErr: `pair "x-a=2"`},
		{name: "brace in value", raw: "X-A=a{b", wantErr: `pair "X-A=a{b"`},
		{name: "closing brace in value", raw: "X-A=a}b", wantErr: `pair "X-A=a}b"`},
		{name: "tab in value", raw: "X-A=a\tb", wantErr: "control character"},
		{name: "newline in value", raw: "X-A=a\nb", wantErr: "control character"},
		{name: "; meant inside a value splits into a bad pair", raw: "X-A=a;b", wantErr: `pair "b" is not Name=value`},
		{name: "; meant inside a value, second half named", raw: "X-A=a;b=c", wantErr: `pair "b=c"`},
		{name: "space in name", raw: "X-A B=1", wantErr: "not allowed in a header name"},
		{name: "colon in name", raw: "X-A:=1", wantErr: "not allowed in a header name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseOutboundSIPHeaders(tt.raw)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parsed %q to %v, want an error containing %q", tt.raw, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), "AICC_OUTBOUND_SIP_HEADERS") || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q must name the variable and contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// A config built in code is held to the parser's rules, ';' included.
func TestValidateOutboundSIPHeadersRejectsSemicolonInAValue(t *testing.T) {
	t.Parallel()
	err := ValidateOutboundSIPHeaders([]SIPHeader{{Name: "X-A", Value: "a;X-B=b"}})
	if err == nil || !strings.Contains(err.Error(), `"X-A=a;X-B=b"`) {
		t.Fatalf("err = %v, want a refusal naming the pair", err)
	}
	if err := ValidateOutboundSIPHeaders([]SIPHeader{{Name: "X-A", Value: "a"}, {Name: "x-a", Value: "b"}}); err == nil {
		t.Error("a case-insensitive duplicate built in code was accepted")
	}
}

func TestValidateOutboundEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		endpoint string
		wantErr  bool
	}{
		{"", false},
		{"sofia/gateway/pstn/%s", false},
		{"sofia/external/%s@carrier.example", false},
		{"loopback/%s/aicc", true},
		{"sofia/gateway/pstn/", true},
		{"sofia/gateway/%s/%s", true},
		{"sofia/gateway/pstn/%d", true},
		{"sofia/gateway/pstn/%s%%", true},
		{"sofia/gateway/pstn/%%", true},
		{"user/%s", true},
	}
	for _, tt := range tests {
		err := ValidateOutboundEndpoint(tt.endpoint)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateOutboundEndpoint(%q) = %v, wantErr %v", tt.endpoint, err, tt.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "AICC_OUTBOUND_ENDPOINT") {
			t.Errorf("error %q does not name the variable", err)
		}
	}
}

func TestLoadRefusesABadOutboundSetting(t *testing.T) {
	t.Setenv("AICC_OUTBOUND_SIP_HEADERS", "X-AICC-Foo=1")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "X-AICC-Foo=1") {
		t.Errorf("Load err = %v, want the bad pair named", err)
	}
	t.Setenv("AICC_OUTBOUND_SIP_HEADERS", "X-FSBC-Out=1;X-Site=bj")
	t.Setenv("AICC_OUTBOUND_ENDPOINT", "loopback/%s/aicc")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AICC_OUTBOUND_ENDPOINT") {
		t.Errorf("Load err = %v, want the endpoint refused", err)
	}
	t.Setenv("AICC_OUTBOUND_ENDPOINT", "sofia/gateway/pstn/%s")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OutboundSIPHeaders) != 2 {
		t.Errorf("headers = %v", cfg.OutboundSIPHeaders)
	}
}
