// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBatchRequestCarriesTheClipAsAWavDataURI(t *testing.T) {
	t.Parallel()
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"output":{"text":"hello there","sentence":[{"text":"hello there","begin_time":120,"end_time":900}]}}`)
	}))
	defer srv.Close()

	c, err := NewDashScopeBatch(srv.URL, "k-123", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	wav := EncodeWAV([]int16{1, 2, 3, 4}, 8000)
	res, err := c.Recognize(context.Background(), BatchRequest{WAV: wav, SampleRate: 8000, LanguageHints: []string{"zh", "en"}})
	if err != nil {
		t.Fatal(err)
	}

	if auth != "Bearer k-123" {
		t.Errorf("Authorization = %q", auth)
	}
	if got["model"] != DefaultBatchModel {
		t.Errorf("model = %v, want the default %q", got["model"], DefaultBatchModel)
	}
	params := got["parameters"].(map[string]any)
	if params["format"] != "wav" || params["sample_rate"] != "8000" {
		t.Errorf("parameters = %v", params)
	}
	if hints := params["language_hints"].([]any); len(hints) != 2 {
		t.Errorf("language_hints = %v", hints)
	}
	msg := got["input"].(map[string]any)["messages"].([]any)[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role = %v", msg["role"])
	}
	part := msg["content"].([]any)[0].(map[string]any)
	if part["type"] != "input_audio" {
		t.Errorf("part type = %v", part["type"])
	}
	data := part["input_audio"].(map[string]any)["data"].(string)
	const prefix = "data:audio/wav;base64,"
	if !strings.HasPrefix(data, prefix) {
		t.Fatalf("audio is not a wav data URI: %.40s", data)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(data, prefix))
	if err != nil || string(raw) != string(wav) {
		t.Errorf("the audio sent is not the clip given (err %v)", err)
	}

	if res.Text != "hello there" || len(res.Sentences) != 1 ||
		res.Sentences[0].StartMs != 120 || res.Sentences[0].EndMs != 900 {
		t.Errorf("result = %+v", res)
	}
}

func TestBatchResponseShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body      string
		sentences int
		text      string
	}{
		"plural key with diarization": {`{"output":{"text":"a b","sentences":[{"text":"a","begin_time":0,"end_time":10,"speaker_id":0},{"text":"b","begin_time":10,"end_time":20}]}}`, 2, "a b"},
		"a single sentence object":    {`{"output":{"text":"a","sentence":{"text":"a","begin_time":5,"end_time":9}}}`, 1, "a"},
		"text only":                   {`{"output":{"text":"just text"}}`, 0, "just text"},
		"no speech":                   {`{"output":{"text":""}}`, 0, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := parseBatchResponse(200, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Sentences) != tc.sentences || res.Text != tc.text {
				t.Errorf("got %+v", res)
			}
		})
	}
}

func TestBatchErrorsSayWhetherToRetry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status    int
		body      string
		retryable bool
	}{
		{429, `{"code":"Throttling.RateQuota","message":"slow down"}`, true},
		{503, `upstream`, true},
		{500, ``, true},
		{400, `{"code":"InvalidParameter","message":"audio too long"}`, false},
		{401, `{"code":"InvalidApiKey","message":"bad key"}`, false},
		{200, `{"code":"InvalidParameter","message":"rejected"}`, false},
		{200, `{"code":"Throttling","message":"later"}`, true},
	}
	for _, tc := range cases {
		_, err := parseBatchResponse(tc.status, []byte(tc.body))
		var be *BatchError
		if !errors.As(err, &be) {
			t.Errorf("%d %s: err = %v, want a BatchError", tc.status, tc.body, err)
			continue
		}
		if be.Retryable != tc.retryable || IsRetryable(err) != tc.retryable {
			t.Errorf("%d %s: retryable = %v, want %v", tc.status, tc.body, be.Retryable, tc.retryable)
		}
	}
	if !IsRetryable(errors.New("connection reset")) {
		t.Error("a transport failure must be retryable")
	}
}

func TestBatchClientRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()
	if _, err := NewDashScopeBatch("wss://x/api-ws/v1/inference", "k", "", nil); err == nil {
		t.Error("the WebSocket endpoint was accepted")
	}
	if _, err := NewDashScopeBatch("https://x/y", "", "", nil); err == nil {
		t.Error("an empty key was accepted")
	}
}

// The vendor answers a clip with no speech in it (hold music, line noise) with
// a 400, measured live on 2026-10-12. That is an empty clip, and only that.
func TestAClipWithNoSpeechIsEmptyNotAnError(t *testing.T) {
	t.Parallel()
	const noWords = `{"request_id":"c631f0b7","code":"CLIENT_ERROR","message":"ASR_RESPONSE_HAVE_NO_WORDS"}`
	res, err := parseBatchResponse(400, []byte(noWords))
	if err != nil || !res.NoSpeech || res.Text != "" || len(res.Sentences) != 0 {
		t.Fatalf("got %+v, %v; want an empty NoSpeech result", res, err)
	}

	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"another CLIENT_ERROR":       {400, `{"code":"CLIENT_ERROR","message":"AUDIO_TOO_LONG"}`},
		"the same words, wrong code": {400, `{"code":"InvalidParameter","message":"ASR_RESPONSE_HAVE_NO_WORDS"}`},
		"the same body on a 500":     {500, noWords},
		"the same body on a 200":     {200, noWords},
	} {
		if _, err := parseBatchResponse(tc.status, []byte(tc.body)); err == nil {
			t.Errorf("%s: no error, want a failure", name)
		}
	}
}
