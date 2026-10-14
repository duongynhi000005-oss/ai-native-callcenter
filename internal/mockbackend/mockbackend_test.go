// SPDX-License-Identifier: Apache-2.0

package mockbackend

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// post calls one endpoint and returns the envelope.
func post(t *testing.T, h http.Handler, path string, body map[string]any) (code string, msg string, data map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
	var out struct {
		RetCode string         `json:"retCode"`
		RetMsg  string         `json:"retMsg"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: bad envelope %q: %v", path, rec.Body.String(), err)
	}
	return out.RetCode, out.RetMsg, out.Data
}

func TestPlanCheckDecidesPerServiceNumber(t *testing.T) {
	t.Parallel()
	h := New(nil)
	for _, tc := range []struct {
		serviceNo, decision, reason string
		hasOptions                  bool
	}{
		{"10086001", "OK_TO_CHANGE", "ELIGIBLE", true},
		{"10086002", "NEED_HUMAN", "IN_CONTRACT_EARLY_TERMINATION_FEE", false},
		{"10086003", "NEED_ID_VERIFY", "CHANGE_REQUIRES_ID_CHECK", true},
		{"10086004", "NEED_HUMAN", "ARREARS", false},
		{"00000000", "NOT_FOUND", "UNKNOWN_SERVICE_NUMBER", false},
	} {
		code, _, data := post(t, h, "/api/plan/check", map[string]any{"serviceNo": tc.serviceNo, "lang": "en"})
		if code != codeOK {
			t.Fatalf("%s: retCode %q; a decision rides a success envelope", tc.serviceNo, code)
		}
		if data["decision"] != tc.decision || data["reason"] != tc.reason {
			t.Errorf("%s: decision %v reason %v, want %s %s", tc.serviceNo, data["decision"], data["reason"], tc.decision, tc.reason)
		}
		if got := data["options"] != ""; got != tc.hasOptions {
			t.Errorf("%s: options = %q, want present=%v", tc.serviceNo, data["options"], tc.hasOptions)
		}
		if data["spoken"] == "" {
			t.Errorf("%s: spoken is empty", tc.serviceNo)
		}
	}
}

func TestPlanCheckAnswersInTheRequestsLanguage(t *testing.T) {
	t.Parallel()
	h := New(nil)
	_, _, en := post(t, h, "/api/plan/check", map[string]any{"serviceNo": "10086001", "lang": "en"})
	_, _, zh := post(t, h, "/api/plan/check", map[string]any{"serviceNo": "10086001", "lang": "zh"})
	if en["spoken"] == zh["spoken"] || en["options"] == zh["options"] {
		t.Errorf("en and zh answers are identical: %v / %v", en, zh)
	}
}

func TestPlanVerifyMatchesTheRegisteredPhone(t *testing.T) {
	t.Parallel()
	h := New(nil)
	for _, tc := range []struct{ serviceNo, last4, want string }{
		{"10086003", "5555", "1"},
		{"10086003", "0000", "0"},
		{"00000000", "5555", "0"},
	} {
		_, _, data := post(t, h, "/api/plan/verify", map[string]any{"serviceNo": tc.serviceNo, "phoneLast4": tc.last4})
		if data["match"] != tc.want {
			t.Errorf("%s/%s: match = %v, want %s", tc.serviceNo, tc.last4, data["match"], tc.want)
		}
	}
}

func TestPlanSubmitReturnsATicketAndStatusProgresses(t *testing.T) {
	t.Parallel()
	h := New(nil)
	code, _, data := post(t, h, "/api/plan/submit", map[string]any{"serviceNo": "10086001", "planId": "2", "lang": "en"})
	if code != codeOK {
		t.Fatalf("submit refused: %s", code)
	}
	ticket, _ := data["ticket"].(string)
	if ticket == "" || data["state"] != "PROCESSING" {
		t.Fatalf("submit = %v, want a ticket in PROCESSING", data)
	}
	_, _, first := post(t, h, "/api/plan/status", map[string]any{"ticket": ticket})
	if first["state"] != "PROCESSING" || first["eta"] == "" {
		t.Errorf("first status = %v, want PROCESSING with an eta", first)
	}
	_, _, second := post(t, h, "/api/plan/status", map[string]any{"ticket": ticket})
	if second["state"] != "DONE" {
		t.Errorf("second status = %v, want DONE", second)
	}
	if code, _, _ := post(t, h, "/api/plan/status", map[string]any{"ticket": "PC-0"}); code == codeOK {
		t.Error("an unknown ticket was reported on")
	}
}

func TestPlanSubmitRefusals(t *testing.T) {
	t.Parallel()
	h := New(nil)
	for name, body := range map[string]map[string]any{
		"plan the address cannot take": {"serviceNo": "10086001", "planId": "3"},
		"account needing an agent":     {"serviceNo": "10086002", "planId": "1"},
		"unknown account":              {"serviceNo": "00000000", "planId": "1"},
		"plan out of range":            {"serviceNo": "10086001", "planId": "9"},
	} {
		if code, msg, _ := post(t, h, "/api/plan/submit", body); code == codeOK || msg == "" {
			t.Errorf("%s: retCode %q msg %q, want a refusal with a message", name, code, msg)
		}
	}
}

func TestKBSearchFindsAndMisses(t *testing.T) {
	t.Parallel()
	h := New(nil)
	for _, tc := range []struct{ query, lang string }{
		{"what is the early termination fee", "en"},
		{"安装费是多少", "zh"},
		{"when will the change take effect", "en"},
		{"how long is the contract", "en"},
		{"gigabit price", "en"},
		{"网速", "zh"},
	} {
		_, _, data := post(t, h, "/api/kb/search", map[string]any{"query": tc.query, "lang": tc.lang})
		if data["found"] != "1" || data["answer"] == "" {
			t.Errorf("%q: %v, want an answer", tc.query, data)
		}
	}
	_, _, miss := post(t, h, "/api/kb/search", map[string]any{"query": "what is the weather", "lang": "en"})
	if miss["found"] != "0" || miss["answer"] != "" {
		t.Errorf("a miss = %v, want found 0 and no answer", miss)
	}
	_, _, en := post(t, h, "/api/kb/search", map[string]any{"query": "installation", "lang": "en"})
	_, _, zh := post(t, h, "/api/kb/search", map[string]any{"query": "installation", "lang": "zh"})
	if en["answer"] == zh["answer"] {
		t.Error("the answer does not follow lang")
	}
}
