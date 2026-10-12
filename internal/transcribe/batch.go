// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultBatchModel is the recorded-speech model post-call transcription uses
// when none is configured.
const DefaultBatchModel = "qwen-audio-3.1-asr-flash"

// BatchRequest is one mono clip for the batch recogniser.
type BatchRequest struct {
	// WAV is a complete mono 16-bit PCM WAV file.
	WAV []byte
	// SampleRate is the clip's rate in Hz, sent as a parameter.
	SampleRate int
	// LanguageHints narrows recognition (at most 4); empty lets the model decide.
	LanguageHints []string
}

// BatchSentence is one recognised sentence; times are relative to the clip.
type BatchSentence struct {
	Text    string
	StartMs int
	EndMs   int
}

// BatchResult is what the recogniser heard in one clip.
type BatchResult struct {
	// Text is the whole clip's text.
	Text string
	// Language as reported by the engine; empty when it does not say.
	Language string
	// Sentences are the timed pieces; empty when the engine returned only Text.
	Sentences []BatchSentence
	// NoSpeech is set when the engine found nothing to transcribe in the clip
	// (music, line noise, a murmur). It is a result, not an error: the clip is
	// simply empty.
	NoSpeech bool
}

// BatchRecognizer recognises a finished clip in one request. It is the batch
// counterpart of Session and deliberately shares nothing with it: one is a
// duplex stream with partials, this is a request that either returns the text
// or fails.
type BatchRecognizer interface {
	Model() string
	Recognize(ctx context.Context, req BatchRequest) (BatchResult, error)
}

// BatchError is a failed batch request. Retryable says whether sending the
// same clip again could succeed: throttling, timeouts and 5xx can, a rejected
// clip cannot.
type BatchError struct {
	Status    int
	Code      string
	Message   string
	Retryable bool
}

func (e *BatchError) Error() string {
	s := "batch recognition"
	if e.Status != 0 {
		s += " http " + strconv.Itoa(e.Status)
	}
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// IsRetryable reports whether err is worth retrying. Anything that is not a
// BatchError (a network failure, a deadline) is.
func IsRetryable(err error) bool {
	var be *BatchError
	if errors.As(err, &be) {
		return be.Retryable
	}
	return true
}

// noWordsMessage is the vendor's message for a clip it heard nothing in.
const noWordsMessage = "ASR_RESPONSE_HAVE_NO_WORDS"

// DashScopeBatch is the synchronous HTTP client for the DashScope
// multimodal-generation endpoint (design 08 section 18, issue #27). One clip
// per request, at most five minutes.
type DashScopeBatch struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

// NewDashScopeBatch builds the client. endpoint is the full HTTPS URL of the
// generation endpoint on the workspace host; it is never the WebSocket one.
func NewDashScopeBatch(endpoint, apiKey, model string, client *http.Client) (*DashScopeBatch, error) {
	if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		return nil, fmt.Errorf("batch endpoint %q must be an http(s) URL", endpoint)
	}
	if apiKey == "" {
		return nil, errors.New("batch recognition needs an API key")
	}
	if model == "" {
		model = DefaultBatchModel
	}
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	return &DashScopeBatch{endpoint: endpoint, apiKey: apiKey, model: model, client: client}, nil
}

// Model returns the model id every request names.
func (c *DashScopeBatch) Model() string { return c.model }

type batchWireAudio struct {
	Data string `json:"data"`
}
type batchWireContent struct {
	Type       string         `json:"type"`
	InputAudio batchWireAudio `json:"input_audio"`
}
type batchWireMessage struct {
	Role    string             `json:"role"`
	Content []batchWireContent `json:"content"`
}

// Recognize sends one clip.
func (c *DashScopeBatch) Recognize(ctx context.Context, req BatchRequest) (BatchResult, error) {
	if len(req.WAV) == 0 {
		return BatchResult{}, &BatchError{Message: "empty clip"}
	}
	params := map[string]any{
		"format":      "wav",
		"sample_rate": strconv.Itoa(req.SampleRate),
	}
	if len(req.LanguageHints) > 0 {
		hints := req.LanguageHints
		if len(hints) > 4 {
			hints = hints[:4]
		}
		params["language_hints"] = hints
	}
	body, err := json.Marshal(map[string]any{
		"model": c.model,
		"input": map[string]any{"messages": []batchWireMessage{{
			Role: "user",
			Content: []batchWireContent{{
				Type:       "input_audio",
				InputAudio: batchWireAudio{Data: "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(req.WAV)},
			}},
		}}},
		"parameters": params,
	})
	if err != nil {
		return BatchResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return BatchResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return BatchResult{}, fmt.Errorf("batch recognition: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return BatchResult{}, fmt.Errorf("batch recognition: reading the response: %w", err)
	}
	logBatchResponse(resp.StatusCode, raw)
	return parseBatchResponse(resp.StatusCode, raw)
}

// logBatchResponse writes a response to AICC_TRANSCRIBE_WIRE, the diagnosis
// directory the streaming client also honours: one file per response, status
// line then body. Responses carry no audio and no credential. Empty writes
// nothing.
func logBatchResponse(status int, raw []byte) {
	dir := os.Getenv("AICC_TRANSCRIBE_WIRE")
	if dir == "" {
		return
	}
	name := filepath.Join(dir, fmt.Sprintf("batch-%d.log", time.Now().UnixNano()))
	_ = os.WriteFile(name, append([]byte(fmt.Sprintf("HTTP %d\n", status)), raw...), 0o600)
}

// batchWireResponse is the part of a successful response this client reads.
//
// Measured 2026-10-12 (design 08 section 18.1): the body carries `text` and
// `sentence` at the top level and the same two again under `output`; this
// reads `output`, the documented place. `sentence` is ONE object, not a list,
// with times in milliseconds from the start of the clip and a `words` array
// that is ignored. With diarization the vendor documents `sentences`, a list
// of the same objects. The response names no language.
type batchWireResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Output  struct {
		Text      string          `json:"text"`
		Sentence  json.RawMessage `json:"sentence"`
		Sentences json.RawMessage `json:"sentences"`
	} `json:"output"`
}

type batchWireSentence struct {
	Text      string   `json:"text"`
	BeginTime *float64 `json:"begin_time"`
	EndTime   *float64 `json:"end_time"`
}

func parseBatchResponse(status int, raw []byte) (BatchResult, error) {
	var r batchWireResponse
	_ = json.Unmarshal(raw, &r)

	// A clip with no recognisable speech is answered with a 400, not an empty
	// 200 (measured 2026-10-12: hold music and line noise, at 8 kHz and 16 kHz).
	// That exact code, and only that one, is an empty result.
	if status == http.StatusBadRequest && r.Code == "CLIENT_ERROR" && r.Message == noWordsMessage {
		return BatchResult{NoSpeech: true}, nil
	}

	if status != http.StatusOK || r.Code != "" {
		be := &BatchError{Status: status, Code: r.Code, Message: r.Message}
		if be.Message == "" {
			be.Message = strings.TrimSpace(string(raw[:min(len(raw), 200)]))
		}
		be.Retryable = status == http.StatusTooManyRequests || status >= 500 || status == http.StatusRequestTimeout ||
			strings.Contains(r.Code, "Throttling") || strings.Contains(r.Code, "Timeout")
		return BatchResult{}, be
	}

	out := BatchResult{Text: strings.TrimSpace(r.Output.Text)}
	var list []batchWireSentence
	switch {
	case isPresent(r.Output.Sentences):
		if err := json.Unmarshal(r.Output.Sentences, &list); err != nil {
			return BatchResult{}, &BatchError{Status: status, Message: "unexpected sentences shape: " + err.Error()}
		}
	case isPresent(r.Output.Sentence):
		var one batchWireSentence
		if err := json.Unmarshal(r.Output.Sentence, &one); err != nil {
			return BatchResult{}, &BatchError{Status: status, Message: "unexpected sentence shape: " + err.Error()}
		}
		list = []batchWireSentence{one}
	}
	var joined []string
	for _, s := range list {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		bs := BatchSentence{Text: text}
		if s.BeginTime != nil {
			bs.StartMs = int(*s.BeginTime)
		}
		if s.EndTime != nil {
			bs.EndMs = int(*s.EndTime)
		}
		out.Sentences = append(out.Sentences, bs)
		joined = append(joined, text)
	}

	// Never lose words. Only one sentence per clip has been observed, so
	// whether a clip of several comes back as a list or as its last sentence
	// alone is unknown; if the sentences do not add up to the text, the text
	// wins and becomes one segment spanning what the sentences cover.
	if len(out.Sentences) > 0 && strings.Join(strings.Fields(strings.Join(joined, " ")), " ") !=
		strings.Join(strings.Fields(out.Text), " ") && out.Text != "" {
		first, last := out.Sentences[0], out.Sentences[len(out.Sentences)-1]
		out.Sentences = []BatchSentence{{Text: out.Text, StartMs: first.StartMs, EndMs: last.EndMs}}
	}
	return out, nil
}

func isPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}
