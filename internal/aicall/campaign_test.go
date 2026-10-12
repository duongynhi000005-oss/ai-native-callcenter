// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"strings"
	"testing"
	"time"
)

//
// A campaign call (X-AICC-Campaign-ID on the bot leg) has no way to a person:
// design 10 section 10.
//

func TestTheCampaignHeadersAreTheCanonicalisedKeys(t *testing.T) {
	// voice.headerTitleCase turns x-aicc-campaign-id into this exact key.
	if headerCampaignID != "X-Aicc-Campaign-Id" || headerAttemptID != "X-Aicc-Attempt-Id" {
		t.Fatalf("headers = %q, %q", headerCampaignID, headerAttemptID)
	}
}

func TestAFailedCampaignCallIsNeverRescuedToTheFallbackQueue(t *testing.T) {
	sw := &fakeSwitch{}
	actions, session, _ := testActions(t, sw)
	queues, err := actions.orchestrator.cfg.Catalog.Queues(t.Context())
	if err != nil {
		t.Fatalf("read queues: %v", err)
	}
	actions.fallbackQueue = &queues[0].ID
	actions.campaignID = "c-1"

	actions.rescueCaller()

	if got := sw.recordedTransfers(); len(got) != 0 {
		t.Errorf("transfers = %v, want none for a campaign call", got)
	}
	if !session.isClosed() {
		t.Error("the session was not closed")
	}
}

func TestAFailedOrdinaryCallIsStillRescuedToTheFallbackQueue(t *testing.T) {
	sw := &fakeSwitch{}
	actions, session, _ := testActions(t, sw)
	queues, err := actions.orchestrator.cfg.Catalog.Queues(t.Context())
	if err != nil {
		t.Fatalf("read queues: %v", err)
	}
	actions.fallbackQueue = &queues[0].ID

	actions.rescueCaller()

	if got := sw.recordedTransfers(); len(got) != 1 {
		t.Errorf("transfers = %v, want the caller rescued", got)
	}
	if !session.isClosed() {
		t.Error("the session was not closed")
	}
}

func TestTheLimitNeverTransfersACampaignCall(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true})
	h.actions.campaignID = "c-1"
	turnBefore := h.session.currentTurn()

	if _, ok := h.actions.openFallbackQueue(t.Context()); ok {
		t.Fatal("a campaign call reports an open fallback queue")
	}

	h.enforce(limitFloorFree)

	if got := h.engine.NodeID(); got != "farewell" {
		t.Fatalf("node = %q, want the closing phase", got)
	}
	h.actions.onPlaybackDone(turnBefore + 1)
	if got := h.sw.recordedTransfers(); len(got) != 0 {
		t.Errorf("transfers = %v, want none", got)
	}
	if got := h.sw.variable("aicc_bot_reason"); got != "" {
		t.Errorf("aicc_bot_reason = %q, want no transfer stamped", got)
	}
	if got := h.sw.variable("aicc_bot_finished"); got != "FLOW_END" {
		t.Errorf("aicc_bot_finished = %q, want FLOW_END", got)
	}
	if h.recorder.hangupCause != "SESSION_LIMIT" {
		t.Errorf("hangup cause = %q, want SESSION_LIMIT", h.recorder.hangupCause)
	}
}

func TestTheLimitHangsUpACampaignCallWithNoClosingPhase(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support"})
	h.actions.campaignID = "c-1"
	turnBefore := h.session.currentTurn()

	h.enforce(limitFloorFree)
	h.actions.onPlaybackDone(turnBefore + 1)

	if got := h.sw.recordedTransfers(); len(got) != 0 {
		t.Errorf("transfers = %v, want none", got)
	}
	if got := h.sw.variable("aicc_bot_finished"); got != "SESSION_LIMIT" {
		t.Errorf("aicc_bot_finished = %q, want SESSION_LIMIT", got)
	}
}

func TestTheWrapUpSteerOffersACampaignCallNoPerson(t *testing.T) {
	h := newLimitHarness(t, limitSetup{queue: "support", hasClosingTarget: true})
	h.actions.campaignID = "c-1"

	h.o.handleWrapUpDue(t.Context(), h.session, h.runtime, h.actions,
		time.Now().Add(3*time.Minute), h.log)

	got := h.instructions()
	if len(got) != 1 || !strings.Contains(got[0], "Do not offer to transfer") {
		t.Fatalf("instructions = %q, want one re-pin that offers no person", got)
	}
}
