// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Tier 2 needs text models: one plays the bot, one plays the caller. This file
// is the whole of what the simulator knows about a model API, the OpenAI chat
// completions wire that DashScope's compatible mode also speaks. It is a test
// tool outside the call path (A1 is about the deployment's voice provider), so
// it is not a provider client and implements nothing of provider.VoiceSession.

// ChatMessage is one message of a chat. Role is system, user, assistant or tool.
type ChatMessage struct {
	Role    string
	Content string
	// ToolCalls are the tools an assistant message asked for.
	ToolCalls []ChatToolCall
	// ToolCallID answers a tool call, on a tool message.
	ToolCallID string
}

// ChatToolCall is a tool the model asked for.
type ChatToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ChatTool describes a tool to the model.
type ChatTool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ChatRequest is one completion.
type ChatRequest struct {
	Model       string
	Messages    []ChatMessage
	Tools       []ChatTool
	Temperature *float64
}

// ChatResponse is the model's answer.
type ChatResponse struct {
	Content          string
	ToolCalls        []ChatToolCall
	PromptTokens     int
	CompletionTokens int
}

// Chat is a text model.
type Chat interface {
	Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// ErrBudget means the invocation's cap on model calls or tokens was reached.
var ErrBudget = errors.New("flowsim: the budget for model calls or tokens is spent")

// Budget wraps a Chat with caps for the whole invocation, so a model that
// loops cannot spend without limit. A zero cap is no cap on that axis.
type Budget struct {
	inner     Chat
	maxCalls  int
	maxTokens int

	mu     sync.Mutex
	calls  int
	tokens int
}

// NewBudget caps inner at maxCalls requests and maxTokens tokens (prompt plus
// completion, counted after each answer, so the last call may overshoot).
func NewBudget(inner Chat, maxCalls, maxTokens int) *Budget {
	return &Budget{inner: inner, maxCalls: maxCalls, maxTokens: maxTokens}
}

// Usage reports what has been spent.
func (b *Budget) Usage() (calls, tokens int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.tokens
}

// Complete implements Chat.
func (b *Budget) Complete(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	b.mu.Lock()
	if (b.maxCalls > 0 && b.calls >= b.maxCalls) || (b.maxTokens > 0 && b.tokens >= b.maxTokens) {
		b.mu.Unlock()
		return ChatResponse{}, ErrBudget
	}
	b.calls++
	b.mu.Unlock()

	resp, err := b.inner.Complete(ctx, req)
	b.mu.Lock()
	b.tokens += resp.PromptTokens + resp.CompletionTokens
	b.mu.Unlock()
	return resp, err
}

// HTTPChat speaks the OpenAI chat completions wire to BaseURL (which ends in
// the API version, e.g. .../compatible-mode/v1). A failed request is an error
// and is never retried: every retry is spend the caps have not seen.
type HTTPChat struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

const chatTimeout = 90 * time.Second

// Complete implements Chat.
func (c *HTTPChat) Complete(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(encodeChat(req))
	if err != nil {
		return ChatResponse{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)

	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("chat request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read chat response: %w", err)
	}
	if resp.StatusCode >= 400 {
		snippet := string(payload)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return ChatResponse{}, fmt.Errorf("chat request: HTTP %d: %s", resp.StatusCode, snippet)
	}
	return decodeChat(payload)
}

func encodeChat(req ChatRequest) map[string]any {
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		entry := map[string]any{"role": m.Role, "content": m.Content}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]map[string]any, len(m.ToolCalls))
			for i, call := range m.ToolCalls {
				calls[i] = map[string]any{"id": call.ID, "type": "function",
					"function": map[string]any{"name": call.Name, "arguments": call.Arguments}}
			}
			entry["tool_calls"] = calls
		}
		messages = append(messages, entry)
	}
	out := map[string]any{"model": req.Model, "messages": messages}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, len(req.Tools))
		for i, t := range req.Tools {
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools[i] = map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": params}}
		}
		out["tools"] = tools
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	return out
}

func decodeChat(payload []byte) (ChatResponse, error) {
	var decoded struct {
		Choices []struct {
			Message struct {
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return ChatResponse{}, fmt.Errorf("chat response is not JSON: %w", err)
	}
	if decoded.Error != nil {
		return ChatResponse{}, fmt.Errorf("chat error: %s", decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("chat response has no choices")
	}
	msg := decoded.Choices[0].Message
	out := ChatResponse{PromptTokens: decoded.Usage.PromptTokens, CompletionTokens: decoded.Usage.CompletionTokens}
	if msg.Content != nil {
		out.Content = *msg.Content
	}
	for i, call := range msg.ToolCalls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		out.ToolCalls = append(out.ToolCalls, ChatToolCall{ID: id, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	return out, nil
}
