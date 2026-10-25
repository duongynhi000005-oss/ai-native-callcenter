// SPDX-License-Identifier: Apache-2.0

// Package wrapupdraft writes an AI draft of an agent's after-call record.
//
// When after-call work begins, one asynchronous job reads that agent's part of
// the live transcript, makes one text-model call and stores the answer on the
// agent's unconfirmed wrap-up row, then tells that agent and nobody else. It is
// strictly additive: it runs on its own goroutine under its own deadline,
// nothing waits for it, and every way it can fail leaves the row exactly as the
// platform opened it.
package wrapupdraft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/events"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/textllm"
)

// Defaults for Config. The note limit is a constant, not a setting: a note
// longer than this is not a wrap-up note.
const (
	DefaultTimeout     = 20 * time.Second
	DefaultBudgetChars = 24000
	DefaultSettle      = 2 * time.Second
	MaxNoteRunes       = 1000
	maxConcurrent      = 8
)

// Store is the slice of the ledger the job needs.
type Store interface {
	ListTranscript(ctx context.Context, callID uuid.UUID) ([]store.TranscriptLine, error)
	ListDispositions(ctx context.Context) ([]store.Disposition, error)
	GetWrapUp(ctx context.Context, callID, agentID uuid.UUID) (store.WrapUp, error)
	SaveWrapUpDraft(ctx context.Context, callID, agentID uuid.UUID, code *string, note string) (store.WrapUp, bool, error)
}

// Publisher is the slice of events.Hub the job publishes through.
type Publisher interface {
	Publish(ctx context.Context, ev events.Event, scope events.Scope) events.Event
}

// Config tunes the job.
type Config struct {
	// Timeout bounds the whole job: settling, reading, the model call, writing.
	Timeout time.Duration
	// BudgetChars is the most characters (runes) the system prompt and the
	// user message may total together.
	BudgetChars int
	// Settle is how long the job waits before reading, so the last lines of a
	// call that is still draining through the transcript actor are in the
	// ledger.
	Settle time.Duration
}

// Drafter runs the jobs.
type Drafter struct {
	store Store
	llm   textllm.Client
	pub   Publisher
	cfg   Config
	log   *slog.Logger

	// after is time.After; tests replace it to drive the clock.
	after func(time.Duration) <-chan time.Time

	slots chan struct{}
	wg    sync.WaitGroup

	mu       sync.Mutex
	inflight map[key]struct{}
}

type key struct{ call, agent uuid.UUID }

// New builds a Drafter. Zero Config values take their defaults.
func New(st Store, llm textllm.Client, pub Publisher, cfg Config, log *slog.Logger) *Drafter {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.BudgetChars <= 0 {
		cfg.BudgetChars = DefaultBudgetChars
	}
	if log == nil {
		log = slog.Default()
	}
	return &Drafter{
		store: st, llm: llm, pub: pub, cfg: cfg, log: log,
		after: time.After, slots: make(chan struct{}, maxConcurrent),
		inflight: map[key]struct{}{},
	}
}

// DraftWrapUp starts the job for one agent's record and returns at once. A job
// already running for the same record makes this a no-op. partyID is the agent
// leg that just ended (uuid.Nil when unknown): its lines are the agent's part
// even if the agent never spoke on it.
func (d *Drafter) DraftWrapUp(callID, agentID, partyID uuid.UUID) {
	k := key{callID, agentID}
	d.mu.Lock()
	if _, running := d.inflight[k]; running {
		d.mu.Unlock()
		return
	}
	d.inflight[k] = struct{}{}
	d.mu.Unlock()

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.mu.Lock()
			delete(d.inflight, k)
			d.mu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				d.log.Error("wrap-up draft panicked", "callId", callID, "agentId", agentID, "panic", r)
			}
		}()
		d.run(callID, agentID, partyID)
	}()
}

// Wait blocks until every started job has finished. For tests and shutdown.
func (d *Drafter) Wait() { d.wg.Wait() }

// errTooLong and friends are the invalid-answer reasons, logged as errors.
var errNoteTooLong = errors.New("the note is longer than allowed")

func (d *Drafter) run(callID, agentID, partyID uuid.UUID) {
	log := d.log.With("callId", callID, "agentId", agentID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timeout := d.after(d.cfg.Timeout)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-timeout:
			cancel()
		case <-stop:
		}
	}()

	if d.cfg.Settle > 0 {
		select {
		case <-d.after(d.cfg.Settle):
		case <-ctx.Done():
			log.Error("wrap-up draft failed", "error", ctx.Err())
			return
		}
	}

	// A record the agent has already confirmed, or that already holds a draft,
	// needs no model call. The write below is still conditional: this is only
	// the cheap exit.
	current, err := d.store.GetWrapUp(ctx, callID, agentID)
	if err != nil {
		log.Error("wrap-up draft failed", "stage", "read the record", "error", err)
		return
	}
	if current.IsConfirmed || current.DraftGeneratedAt != nil {
		log.Info("wrap-up draft skipped", "reason", "record already confirmed or drafted")
		return
	}

	lines, err := d.store.ListTranscript(ctx, callID)
	if err != nil {
		log.Error("wrap-up draft failed", "stage", "read the transcript", "error", err)
		return
	}
	seg := split(lines, agentID, partyID)
	if len(seg.agent) == 0 {
		log.Info("wrap-up draft skipped", "reason", "no transcript of the agent's part")
		return
	}
	dispositions, err := d.store.ListDispositions(ctx)
	if err != nil {
		log.Error("wrap-up draft failed", "stage", "read the dispositions", "error", err)
		return
	}
	prompt, ok := render(seg, dispositions, d.cfg.BudgetChars)
	if !ok {
		log.Info("wrap-up draft skipped", "reason", "transcript does not fit the prompt budget",
			"budgetChars", d.cfg.BudgetChars)
		return
	}

	select {
	case d.slots <- struct{}{}:
		defer func() { <-d.slots }()
	case <-ctx.Done():
		log.Error("wrap-up draft failed", "stage", "wait for a slot", "error", ctx.Err())
		return
	}
	answer, err := d.llm.Complete(ctx, textllm.Request{System: systemPrompt, User: prompt, JSON: true})
	if err != nil {
		log.Error("wrap-up draft failed", "stage", "text model", "error", err)
		return
	}
	code, note, err := parseAnswer(answer, dispositions)
	if err != nil {
		log.Error("wrap-up draft failed", "stage", "invalid answer", "error", err)
		return
	}
	if code == nil && note == "" {
		log.Info("wrap-up draft skipped", "reason", "the model had nothing to suggest")
		return
	}

	saved, wrote, err := d.store.SaveWrapUpDraft(ctx, callID, agentID, code, note)
	if err != nil {
		log.Error("wrap-up draft failed", "stage", "write the draft", "error", err)
		return
	}
	if !wrote {
		log.Info("wrap-up draft skipped", "reason", "record confirmed, already drafted or missing")
		return
	}
	log.Info("wrap-up draft written", "hasDisposition", saved.DraftDispositionCode != "",
		"disposition", saved.DraftDispositionCode, "noteRunes", utf8.RuneCountInString(note))
	d.publish(callID, agentID, saved)
}

// answer is the model's JSON.
type answer struct {
	DispositionCode *string `json:"dispositionCode"`
	Note            *string `json:"note"`
}

// parseAnswer validates the model's reply against the codes it was offered. A
// code outside that list is an invalid answer, not a draft with the code
// dropped: the model was told the list, and a reply that ignores it cannot be
// trusted about the note either.
func parseAnswer(raw string, offered []store.Disposition) (code *string, note string, err error) {
	var a answer
	if err := json.Unmarshal([]byte(stripFence(raw)), &a); err != nil {
		return nil, "", fmt.Errorf("the reply is not a JSON object: %w", err)
	}
	if a.Note != nil {
		note = strings.TrimSpace(*a.Note)
	}
	if utf8.RuneCountInString(note) > MaxNoteRunes {
		return nil, "", fmt.Errorf("%w (%d > %d characters)", errNoteTooLong,
			utf8.RuneCountInString(note), MaxNoteRunes)
	}
	if a.DispositionCode != nil {
		c := strings.TrimSpace(*a.DispositionCode)
		if c != "" {
			found := false
			for _, d := range offered {
				if d.Code == c {
					found = true
					break
				}
			}
			if !found {
				return nil, "", fmt.Errorf("disposition code %q is not one of the offered codes", c)
			}
			code = &c
		}
	}
	return code, note, nil
}

// stripFence removes a markdown code fence some models wrap JSON in despite
// being asked for a bare object.
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimPrefix(s, "json")
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

// publish tells the agent whose record it is. The scope is that agent alone:
// a draft of one agent's wrap-up is not for a colleague on the same call.
func (d *Drafter) publish(callID, agentID uuid.UUID, saved store.WrapUp) {
	if d.pub == nil {
		return
	}
	payload := map[string]any{
		"callId": callID,
		"note":   "",
	}
	if saved.DraftNote != nil {
		payload["note"] = *saved.DraftNote
	}
	if saved.DraftDispositionCode != "" {
		payload["dispositionCode"] = saved.DraftDispositionCode
	}
	if saved.DraftGeneratedAt != nil {
		payload["generatedAt"] = saved.DraftGeneratedAt.UTC()
	}
	d.pub.Publish(context.Background(), events.Event{
		Type:    events.TypeAgentWrapUpDrafted,
		CallID:  &callID,
		AgentID: &agentID,
		Payload: payload,
	}, events.Scope{AgentIDs: []uuid.UUID{agentID}})
}
