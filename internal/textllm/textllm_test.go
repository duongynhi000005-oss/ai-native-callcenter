// SPDX-License-Identifier: Apache-2.0

package textllm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompleteSendsAChatRequestAndReturnsTheText(t *testing.T) {
	var got chatRequest
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"note\":\"hi\"}"}}]}`))
	}))
	defer srv.Close()

	c, err := New(Config{Endpoint: srv.URL + "/v1/", Model: "m", APIKey: "k"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	text, err := c.Complete(context.Background(), Request{System: "sys", User: "usr", JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if text != `{"note":"hi"}` {
		t.Errorf("text = %q", text)
	}
	if path != "/v1/chat/completions" || auth != "Bearer k" {
		t.Errorf("path %q auth %q", path, auth)
	}
	if got.Model != "m" || len(got.Messages) != 2 || got.Messages[0].Role != "system" ||
		got.ResponseFormat["type"] != "json_object" {
		t.Errorf("request = %+v", got)
	}
}

func TestCompleteReportsAnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer srv.Close()
	c, _ := New(Config{Endpoint: srv.URL, Model: "m", APIKey: "k"}, nil)
	_, err := c.Complete(context.Background(), Request{User: "u"})
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "slow down") {
		t.Errorf("err = %v", err)
	}
}

func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	for _, cfg := range []Config{{}, {Endpoint: "http://x", Model: "m"}, {Endpoint: "ftp://x", Model: "m", APIKey: "k"}} {
		if _, err := New(cfg, nil); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
}
