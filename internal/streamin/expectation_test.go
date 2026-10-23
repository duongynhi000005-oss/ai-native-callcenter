// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// isExpecting reports whether the watchdog is still armed for an attach.
func (s *Server) isExpecting(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[token] != nil
}

// isExpectingAny reports whether any attach is waiting to connect.
func (s *Server) isExpectingAny() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) > 0
}

// hasArrived reports whether a stream dialled back before its attach armed an
// expectation, and is still waiting to be claimed by it.
func (s *Server) hasArrived(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, here := s.arrivals[token]
	return here
}

// expireExpectations fires every armed connect-grace check now, so a test can
// assert what it reports without waiting twelve seconds for a real timer. It
// does what the timer does, in the same order: stop it, then hand the check the
// expectation it was armed for.
func (s *Server) expireExpectations() {
	s.mu.Lock()
	type armed struct {
		token string
		exp   *expectation
	}
	live := make([]armed, 0, len(s.pending))
	for token, exp := range s.pending {
		live = append(live, armed{token: token, exp: exp})
	}
	s.mu.Unlock()
	for _, a := range live {
		a.exp.timer.Stop()
		s.giveUpOn(a.token, a.exp)
	}
}

// tokenOf is the token a tap put on the URL it gave the switch.
func tokenOf(wsURL string) string {
	u, err := url.Parse(wsURL)
	if err != nil {
		return ""
	}
	return u.Query().Get("t")
}

// A stream that dialled back and was turned away here is not also reported,
// twelve seconds later, as one that never connected.
//
// Those are different faults with different causes — the second means the
// switch never called back at all, which is a network or module problem — and
// an operator handed both accounts of one event goes looking for something
// that is not there. Seen live on 2026-08-26: three click-to-dial calls, each
// refused for want of a transcript actor and each reported three separate
// times.
//
// Asserted on the expectation rather than on log output, and without waiting
// out connectGrace: what the fix does is disarm the watchdog, and the log line
// is downstream of that.
func TestARefusedStreamIsNotAlsoReportedAsNeverConnected(t *testing.T) {
	t.Parallel()
	// testServer's Transcripts never has an actor, which is exactly the
	// production case: a transcript actor is created only by the bot path.
	s := testServer(t)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })

	claim := Claim{
		CallID: uuid.New(), PartyID: uuid.New(), AgentID: uuid.New(),
		Channel: "agent-chan", Expires: time.Now().Add(time.Minute),
	}
	token := s.Token(claim)

	s.Expect(token, claim)
	if !s.isExpecting(token) {
		t.Fatal("the attach armed no watchdog; the rest of this proves nothing")
	}

	dial := "ws://" + s.Addr() + StreamPath + "?t=" + url.QueryEscape(token)
	conn, _, err := websocket.DefaultDialer.Dial(dial, nil)
	if err != nil {
		t.Fatalf("the tap could not reach the ingest at all: %v", err)
	}
	defer conn.Close()

	waitUntil(t, func() bool { return !s.isExpecting(token) },
		"the refusal to disarm the watchdog")
}
