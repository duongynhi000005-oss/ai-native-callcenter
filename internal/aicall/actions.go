// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/campaign"
	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// callActions is what one call's flow tools actually do.
//
// The invariant all of them share: an action that ends or moves the call never
// runs while the bot is mid-sentence. The tool returns immediately — the model
// needs the result to speak its closing line — and the action itself is armed,
// to run when that line has finished playing, with a cap so a stalled provider
// cannot hold the caller hostage.
type callActions struct {
	orchestrator *Orchestrator
	session      *Session
	log          *slog.Logger

	// callerChannel is the caller's leg on the switch — the one that survives
	// this conversation and goes on to a queue.
	callerChannel string
	fallbackQueue *uuid.UUID
	// campaignID and attemptID come from X-AICC-Campaign-ID / X-AICC-Attempt-ID
	// on the bot leg; a non-empty campaignID marks a campaign call, which has no
	// way to a person (isCampaign). Zero on every other call.
	campaignID string
	attemptID  string
	// recorder and facts feed the ledger; both may be nil in tests.
	recorder *callRecorder
	facts    *callFacts

	// graceCap overrides actionGraceCap; zero keeps it. Tests only.
	graceCap time.Duration

	mu    sync.Mutex
	armed func()
	// isArmedTransfer says the armed ending is a transfer to an agent.
	isArmedTransfer bool
	// armedInTurn is the model turn in progress when the action was armed —
	// the turn the arming tool call arrived in. A tool arms inside its own
	// dispatch, and a terminal phase whose line the tool result carries arms
	// before the result is sent, so in both cases the turn the answer produces
	// is a later one. The
	// action waits for the playback of a LATER turn: the closing line is spoken
	// in the turn created by the tool's own result (or, on a client that speaks
	// text outright, in the line's own turn after it), and the turn that
	// carried the tool call may finish playing after arming — acting on that
	// one would cut in before the line is spoken.
	armedInTurn int
	// isLineSpoken records that a later turn has finished generating — the
	// closing line exists and is playing out or already played. Only a turn
	// that ran to completion counts: a turn cut short before its words existed
	// (a tool-result turn stopped to make room for a line, or one the caller
	// talked over) is not the line, and counting it let a barge-in fire the
	// action while the real line was still being generated. A caller cutting
	// in early on the line resets it (onBargeIn).
	isLineSpoken bool
	// isInterrupted says the caller cut in on the armed closing line with more
	// than closingTailAck of it unplayed (onBargeIn). From then on the ending
	// waits for a turn that plays audio (onPlaybackDone), and the model's first
	// bare hangup is refused (Hangup).
	isInterrupted bool
	// audioTurnsAtInterrupt is Session.audioProgress's finished-turn count when
	// the interruption was seen; a larger count later means the model has
	// answered aloud since.
	audioTurnsAtInterrupt int
	// isHangupRefused says the one refusal has been spent; a second hangup
	// succeeds.
	isHangupRefused bool
	// armedGeneration lets a cap timer recognise the action it belongs to, and
	// retires the timer when the cap is moved.
	armedGeneration int
	// capDue is when the running cap fires.
	capDue time.Time
	// isEndingLocked says the call's ending has been decided by the platform
	// (the flow's time limit) and nothing the model does may replace it: arm
	// keeps what is armed, and the ending tools report success without acting.
	// Unlike an armed action it outlives the running of that action, so a
	// model answering the handover cue with hangup after the transfer has run
	// cannot arm a second ending.
	isEndingLocked bool
}

// TransferToAgent checks the queue can take the caller, then arms the
// transfer to run once the bridge line has been heard.
func (a *callActions) TransferToAgent(ctx context.Context, request flow.TransferRequest) (flow.Result, error) {
	if a.isEndingDecided() {
		a.log.Info("transfer ignored: the call's time limit has already decided how it ends",
			"asked", request.Queue)
		return flow.Succeeded(nil, ""), nil
	}
	if isArmed, isTransfer := a.armedState(); isArmed {
		a.log.Info("transfer ignored: the call's ending is already armed",
			"asked", request.Queue, "isArmedTransfer", isTransfer)
		if isTransfer {
			// The model repeating itself: the transfer it wants is already on
			// its way, and a second one would restamp the channel and restart
			// the wait.
			return flow.Succeeded(nil, ""), nil
		}
		// The armed ending is a hangup. Reporting success here had the model
		// promise a transfer that never came (#147: "You'll be connected to an
		// agent", then the line dropped). A refusal is a conversation.
		return flow.Failed("CALL_ENDING", a.endingHint(false)), nil
	}
	queue, ok := a.queueForTransfer(ctx, request.Queue)
	if !ok {
		// The refusal is a conversation, not an error: the bot explains and
		// offers what it can still do.
		a.log.Warn("transfer refused: no queue to send the caller to",
			"asked", request.Queue, "hasFallback", a.fallbackQueue != nil)
		return flow.Failed("QUEUE_UNKNOWN", a.refusalHint()), nil
	}
	if !queue.IsEnabled {
		a.log.Warn("transfer refused: the queue is disabled", "queue", queue.Name)
		return flow.Failed("QUEUE_CLOSED", a.refusalHint()), nil
	}
	if a.callerChannel == "" {
		return flow.Result{}, errNoCallerChannel
	}

	// The summary rides the caller's channel into the queue, so the agent who
	// answers already knows what the conversation established.
	a.stampChannel("aicc_bot_summary", request.Summary)
	a.stampChannel("aicc_bot_reason", request.Reason)
	if len(request.Slots) > 0 {
		if encoded, err := json.Marshal(request.Slots); err == nil {
			a.stampChannel("aicc_bot_slots", string(encoded))
		}
	}
	// The ledger follows the call: after a transfer the human path writes the
	// one CDR, and the bot's share of the story goes with the caller.
	if a.recorder != nil {
		a.recorder.markTransferred(request.Summary, request.Reason)
		if a.facts != nil {
			a.stampBotShare(a.facts)
		}
	}

	a.armAs(ctx, true, func() {
		a.log.Info("transferring the caller", "queue", queue.Name, "ext", queue.ExtNumber)
		// Last, because they are the only ones of the bot's stamps still
		// changing.
		if a.recorder != nil {
			a.recorder.markHandedOver(queue.ID)
			a.stampUnbackedClaims(a.recorder)
			a.stampBotSec(a.recorder)
		}
		a.handOnCaller()
		// No context: the switch knows which dialplan this deployment runs in,
		// and naming one here is how the bot came to hand its callers to a
		// context where the queue extension does not exist. They heard hold
		// music and joined nothing.
		if err := a.orchestrator.cfg.Switch.TransferToExtension(
			a.callerChannel, queue.ExtNumber, ""); err != nil {
			a.log.Error("transfer failed", "queue", queue.Name, "error", err)
		}
		// Our SIP leg's job is done either way; the caller's leg has moved on.
		a.session.Close(context.Background())
	})

	return flow.Succeeded(map[string]any{"queue": queue.Name}, ""), nil
}

// queueForTransfer resolves where the caller is being put through.
//
// The name the model gave, when the deployment has it. Otherwise the number's
// own fallback queue: the caller asked for a person, and refusing them over an
// argument the bot itself got wrong serves nobody. The queue argument is an
// enum of the real queues now, so this is the narrow path — a flow written
// against another deployment's queue names, or a queue deleted mid-call — but
// it is the path where a caller would otherwise be dropped.
func (a *callActions) queueForTransfer(ctx context.Context, name string) (catalog.Queue, bool) {
	if queue, ok := a.orchestrator.findQueue(ctx, name); ok {
		return queue, true
	}
	if a.fallbackQueue == nil {
		return catalog.Queue{}, false
	}
	queues, err := a.orchestrator.cfg.Catalog.Queues(ctx)
	if err != nil {
		a.log.Error("read queues", "error", err)
		return catalog.Queue{}, false
	}
	for _, queue := range queues {
		if queue.ID != *a.fallbackQueue {
			continue
		}
		a.log.Warn("the model named a queue this deployment does not have; "+
			"using the number's fallback queue",
			"asked", name, "using", queue.Name)
		return queue, true
	}
	return catalog.Queue{}, false
}

// TakeMessage records what the caller wants passed on as an OPEN callback,
// which is the work queue an agent later rings through.
func (a *callActions) TakeMessage(ctx context.Context, request flow.MessageRequest) (flow.Result, error) {
	a.log.Info("message taken",
		"message", request.Message, "callbackNumber", request.CallbackNumber)

	if ledger := a.orchestrator.cfg.Ledger; ledger != nil && a.recorder != nil {
		phoneNumber := request.CallbackNumber
		if phoneNumber == "" && a.facts != nil {
			phoneNumber = a.facts.fromNumber
		}
		callID := a.recorder.callID
		callback, err := ledger.InsertCallback(ctx, &callID, a.fallbackQueue,
			phoneNumber, request.Message)
		if err != nil {
			a.log.Error("could not save the callback", "error", err)
			return flow.Failed("SAVE_FAILED", a.refusalHint()), nil
		}
		if announce := a.orchestrator.cfg.AnnounceCallback; announce != nil {
			announce(callback)
		}
	}
	return flow.Succeeded(nil, ""), nil
}

// RecordLead stores the lead on the call's attempt, overwriting an earlier one.
// Nothing else happens now: the DNC row and the callback are written once, from
// the final value, when the attempt finishes (design 10 section 11), so a model
// that changes its mind leaves nothing to undo and the call is not ended.
func (a *callActions) RecordLead(ctx context.Context, request flow.LeadRequest) (flow.Result, error) {
	if !a.isCampaign() || a.orchestrator.cfg.Leads == nil || a.recorder == nil {
		// The runtime offers the tool on a campaign call only, so this is a
		// wiring gap; the model is told the lead was not saved.
		a.log.Error("lead not recorded: no place to record it", "intent", request.Intent)
		return flow.Failed("NOT_RECORDED", a.leadHint()), nil
	}
	ok, err := a.orchestrator.cfg.Leads.SetLead(ctx, a.recorder.callID,
		campaign.LeadIntent(request.Intent), request.Notes)
	if err != nil {
		a.log.Error("could not save the lead", "intent", request.Intent, "error", err)
		return flow.Failed("SAVE_FAILED", a.leadHint()), nil
	}
	if !ok {
		a.log.Warn("lead not saved: the attempt is no longer in flight", "intent", request.Intent)
		return flow.Failed("NOT_RECORDED", a.leadHint()), nil
	}
	a.log.Info("lead recorded", "intent", request.Intent,
		"notesRunes", len([]rune(request.Notes)))
	return flow.Succeeded(nil, ""), nil
}

// leadHint steers the model after a lead could not be saved: it carries on and
// says nothing of it, because the customer has no use for our bookkeeping.
func (a *callActions) leadHint() string {
	if a.session != nil && a.session.cfg.Session.Language == "zh" {
		return "记录未保存。不要向客户提及，继续当前对话；如有必要稍后可再次调用。"
	}
	return "The record was not saved. Do not mention it to the customer; carry on " +
		"with the conversation, and you may try again later if it still matters."
}

// Hangup ends the call once the goodbye has been heard.
func (a *callActions) Hangup(ctx context.Context, _ flow.HangupRequest) (flow.Result, error) {
	if a.isEndingDecided() {
		a.log.Info("hangup ignored: the call's time limit has already decided how it ends")
		return flow.Succeeded(nil, ""), nil
	}
	if isArmed, isTransfer := a.armedState(); isArmed {
		// The ending already armed stands, and the ledger keeps its outcome.
		a.log.Info("hangup ignored: the call's ending is already armed",
			"isArmedTransfer", isTransfer)
		if isTransfer {
			// The caller is being put through; saying the call is over would
			// contradict that. Told so, the model says at most a short line.
			return flow.Failed("CALL_ENDING", a.endingHint(true)), nil
		}
		if a.refuseBareHangup() {
			return flow.Failed("CALL_ENDING", a.interruptedHangupHint()), nil
		}
		// A repeated hangup is the model's intent exactly; the ending runs on
		// the playback of a turn that played audio (onPlaybackDone).
		return flow.Succeeded(nil, ""), nil
	}
	if a.recorder != nil {
		a.recorder.markHangup()
	}
	a.arm(ctx, func() {
		a.log.Info("hanging up after the farewell")
		a.markFinished("HANGUP")
		a.session.Close(context.Background())
	})
	return flow.Succeeded(nil, ""), nil
}

// arm schedules an action for the end of the closing line the model is about
// to speak, with a cap in case that line never finishes.
//
// The first ending armed is the call's ending: arming again while it waits
// changes nothing. Replacing it moved armedInTurn and restarted the cap on
// every repeat, so a model that called transfer_to_agent three times waited
// for a turn after the last call and hit the cap ten seconds after it (#25).
func (a *callActions) arm(ctx context.Context, action func()) {
	a.armAs(ctx, false, action)
}

// armAs is arm, recording whether the ending is a transfer to an agent, which
// decides how a later hangup or transfer from the model is answered.
func (a *callActions) armAs(_ context.Context, isTransfer bool, action func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isEndingLocked {
		a.log.Info("not arming: the call's time limit has already decided how it ends")
		return
	}
	if a.armed != nil {
		a.log.Info("not arming: the call's ending is already armed")
		return
	}
	a.armed = action
	a.isArmedTransfer = isTransfer
	a.armedInTurn = a.session.currentTurn()
	a.isLineSpoken = false
	a.isInterrupted = false
	a.isHangupRefused = false
	graceCap := a.graceCap
	if graceCap <= 0 {
		graceCap = actionGraceCap
	}
	a.startCapLocked(graceCap)
}

// startCapLocked (re)starts the cap, retiring any timer already running.
func (a *callActions) startCapLocked(after time.Duration) {
	a.armedGeneration++
	generation := a.armedGeneration
	a.capDue = time.Now().Add(after)
	time.AfterFunc(after, func() { a.runAtCap(generation) })
}

// runAtCap runs the armed action when its closing line has not been heard in
// time. The two ways to get here are told apart: a line that was never
// generated is a provider that went quiet, while a line generated but not
// heard is playback that ran past the cap's estimate.
func (a *callActions) runAtCap(generation int) {
	a.mu.Lock()
	armed := a.armed
	isStillArmed := armed != nil && a.armedGeneration == generation
	isLineSpoken := a.isLineSpoken
	if isStillArmed {
		a.armed = nil
	}
	a.mu.Unlock()
	if !isStillArmed {
		return
	}
	if isLineSpoken {
		a.log.Warn("running an armed action at the cap; the closing line was generated " +
			"but its playback had not finished")
	} else {
		a.log.Warn("running an armed action at the cap; no closing line was generated " +
			"after the action was armed")
	}
	armed()
}

// isArmed reports whether an action is already waiting for the closing line.
func (a *callActions) isArmed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.armed != nil
}

// armedState reports whether an ending is armed and whether it is a transfer.
func (a *callActions) armedState() (isArmed, isTransfer bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.armed != nil, a.isArmedTransfer
}

// endingHint steers the model after it asked for a transfer or a hangup the
// armed ending already settles. The call is ending, the answer to the caller
// is short, and no transfer is promised unless one is armed.
func (a *callActions) endingHint(isHangupAsked bool) string {
	zh := a.session != nil && a.session.cfg.Session.Language == "zh"
	if isHangupAsked {
		if zh {
			return "来电者正在被转接给人工,通话不会挂断。不要道别,最多简短说明正在为他们转接。"
		}
		return "The caller is already being transferred to an agent, so the call will not " +
			"be hung up. Do not say goodbye; at most say briefly that they are being connected."
	}
	if zh {
		return "通话已经在结束,不会转接人工。不要承诺转接;简短回答对方,或直接道别。"
	}
	return "The call is already ending and will not be transferred. Do not promise a " +
		"transfer; answer the caller briefly or just say goodbye."
}

// refuseBareHangup reports whether this hangup is the one to refuse: the caller
// interrupted the armed closing line, the model has said nothing aloud since
// (in an earlier turn or so far in this one), and the refusal is unspent.
// Without it the model answered a caller's question with a bare hangup and the
// call ended on the audio-less turn, the question unanswered (#147). It refuses
// once, so a model that insists gets its hangup and there is no loop. The cap,
// restarted here, bounds the wait for the answer: a model that never speaks
// still ends the call.
func (a *callActions) refuseBareHangup() bool {
	finished, isSpeaking := a.session.audioProgress()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.armed == nil || !a.isInterrupted || a.isHangupRefused ||
		isSpeaking || finished > a.audioTurnsAtInterrupt {
		return false
	}
	a.isHangupRefused = true
	graceCap := a.graceCap
	if graceCap <= 0 {
		graceCap = actionGraceCap
	}
	a.startCapLocked(graceCap)
	return true
}

// interruptedHangupHint tells the model to answer the caller who spoke over
// its closing line, and that the call ends on its own after the answer.
func (a *callActions) interruptedHangupHint() string {
	if a.session != nil && a.session.cfg.Session.Language == "zh" {
		return "来电者打断了你的结束语。请用一句简短的话回应他们刚才说的内容,通话会在这句话之后自动结束。不要调用 hangup。"
	}
	return "The caller spoke over your closing line. Reply to what they said in one short " +
		"sentence; the call ends by itself right after it. Do not call hangup."
}

// lockEnding keeps the ending armed now — or already run — as the call's
// ending, whatever the model asks for afterwards.
func (a *callActions) lockEnding() {
	a.mu.Lock()
	a.isEndingLocked = true
	a.mu.Unlock()
}

// isEndingDecided reports whether lockEnding has been called.
func (a *callActions) isEndingDecided() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.isEndingLocked
}

// runArmedNow runs the armed action without waiting for a line, for an ending
// that has no line to wait for.
func (a *callActions) runArmedNow() {
	a.mu.Lock()
	armed := a.armed
	a.armed = nil
	a.mu.Unlock()
	if armed != nil {
		armed()
	}
}

// openFallbackQueue is the number's own queue when it exists and is taking
// calls: the one queue the platform may hand a caller to on its own.
func (a *callActions) openFallbackQueue(ctx context.Context) (catalog.Queue, bool) {
	if a.isCampaign() {
		return catalog.Queue{}, false
	}
	if a.fallbackQueue == nil {
		return catalog.Queue{}, false
	}
	queues, err := a.orchestrator.cfg.Catalog.Queues(ctx)
	if err != nil {
		a.log.Error("read queues", "error", err)
		return catalog.Queue{}, false
	}
	for _, queue := range queues {
		if queue.ID == *a.fallbackQueue {
			return queue, queue.IsEnabled
		}
	}
	return catalog.Queue{}, false
}

// disarm drops whatever was waiting to be spoken over, because the call it
// belonged to has ended. The cap that exists to rescue a closing line that
// never finishes cannot tell that case from a caller who hung up in the middle
// of one — and there it fired ten seconds after the caller was gone, onto a
// channel that no longer existed: two warnings and a "transfer failed" that
// reads exactly like a real one.
func (a *callActions) disarm() {
	a.mu.Lock()
	a.armed = nil
	a.armedGeneration++
	a.mu.Unlock()
}

// onPlaybackDone runs whatever was armed, now that the caller has heard the
// line that precedes it. turn says whose playback this was: the turn the tool
// call arrived in does not count, only a turn the tool's result created.
//
// hasAudio says the turn played something to the caller. It is required only
// once the caller has interrupted the closing line: a model that answers that
// interruption with a bare tool call produces an audio-less turn whose
// playback "ends" at once, and running the ending on it hung up on a question
// nobody had answered (#147). Before an interruption the rule stays as it was
// (the line's own turn carries audio, and an audio-less repeat of hangup in it
// must still end the call), and the "bye now" acknowledgement never reaches
// here (onBargeIn runs it directly). The cap bounds a model that never speaks.
func (a *callActions) onPlaybackDone(turn int, hasAudio bool) {
	a.mu.Lock()
	if a.armed == nil || turn <= a.armedInTurn || (a.isInterrupted && !hasAudio) {
		a.mu.Unlock()
		return
	}
	armed := a.armed
	a.armed = nil
	a.mu.Unlock()
	armed()
}

// onTurnDone records that the closing line has finished generating. A turn
// that was cut short is not the line, whichever turn it was.
//
// The line's length is known from here on, so the cap is moved to cover it.
// The cap started at arming, but the line plays behind whatever is still
// queued: the turn that carried the tool call can go on speaking for seconds
// after the call, and a cap counted from arming then cut the tail of the line
// off (issue #24). The cap only ever moves later; a line that fits inside it
// keeps the original.
//
// Every later turn does this, not only the first: a caller who interrupts the
// closing line gets an answer, and a model may answer in two turns (#147: a
// second answer clipped by a cap that only the first had moved).
func (a *callActions) onTurnDone(turn int, isInterrupted bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.armed == nil || turn <= a.armedInTurn || isInterrupted {
		return
	}
	a.isLineSpoken = true
	playout := a.session.queuedPlayout() + lineDrainMargin
	if due := time.Now().Add(playout); due.After(a.capDue) {
		a.startCapLocked(playout)
	}
}

// closingTailAck is how much of the closing line may still be unplayed when
// the caller speaks for that speech to count as answering it. Caller speech
// with at most this much of the line left ("bye now" over its last words) is
// the acknowledgement the call ends on; with more left it is an interruption
// and the line has not been said. 1.5 s is the last phrase or two of a
// goodbye: short enough that a question cannot have been cut off in the middle
// of the line, long enough to cover the lag between the caller starting to
// answer and the detector reporting it.
const closingTailAck = 1500 * time.Millisecond

// onBargeIn decides what the caller taking the floor means for an armed ending.
// unplayed is the audio still queued for the caller when they spoke, read
// before the barge-in flushed it.
//
// Over or before a line that has not finished generating, nothing changes: the
// ending waits for a line.
//
// Over a line that finished generating, there are two cases, told apart by how
// much of it the caller had left to hear:
//
//   - At most closingTailAck: the caller is answering the goodbye ("bye now").
//     That is what this exists for: the detector reports speech, the playback
//     watch is superseded, PLAYBACK_DONE never arrives, and every such call
//     used to end on the grace cap, five silent seconds late. Act now.
//   - More: the caller cut in on the line, with a question (#147: "wait, what's
//     the trial offer?" 2.3 s into a 9 s line, and the call was ended on it).
//     The flush and truncation the barge-in does proceed, the line no longer
//     counts as said, the ending stays armed, and the model answers in a later
//     turn whose playback (or, if the caller cuts that one short too, the same
//     rule) runs it. The cap starts over so that answer has the time to be made.
func (a *callActions) onBargeIn(unplayed time.Duration) {
	a.mu.Lock()
	if a.armed == nil || !a.isLineSpoken {
		a.mu.Unlock()
		return
	}
	if unplayed > closingTailAck {
		a.isLineSpoken = false
		a.isInterrupted = true
		a.audioTurnsAtInterrupt, _ = a.session.audioProgress()
		graceCap := a.graceCap
		if graceCap <= 0 {
			graceCap = actionGraceCap
		}
		a.startCapLocked(graceCap)
		a.mu.Unlock()
		a.log.Info("caller spoke over the closing line; answering them before the armed action runs",
			"unplayedMs", unplayed.Milliseconds())
		return
	}
	armed := a.armed
	a.armed = nil
	a.mu.Unlock()
	a.log.Info("caller spoke after the closing line; running the armed action now",
		"unplayedMs", unplayed.Milliseconds())
	armed()
}

// campaignDeniedTools are the built-ins a campaign call never offers: a
// campaign call has no way to a person, and callbacks come only from leads.
var campaignDeniedTools = []string{"transfer_to_agent", "take_message"}

// isCampaign reports whether this call was placed by a campaign.
func (a *callActions) isCampaign() bool { return a.campaignID != "" }

// rescueCaller moves the caller to the number's fallback queue after a failed
// conversation.
func (a *callActions) rescueCaller() {
	if a.callerChannel == "" || a.fallbackQueue == nil || a.isCampaign() {
		a.session.Close(context.Background())
		return
	}
	queues, err := a.orchestrator.cfg.Catalog.Queues(context.Background())
	if err == nil {
		for _, queue := range queues {
			if queue.ID == *a.fallbackQueue {
				a.log.Info("rescuing the caller to the fallback queue", "queue", queue.Name)
				a.handOnCaller()
				if err := a.orchestrator.cfg.Switch.TransferToExtension(
					a.callerChannel, queue.ExtNumber, ""); err != nil {
					a.log.Error("rescue transfer failed", "error", err)
				}
				break
			}
		}
	}
	a.session.Close(context.Background())
}

// markFinished tells the dialplan that this call ended because the bot decided
// it had, and not because the bot disappeared.
//
// The inbound script cannot tell those apart on its own: both look like a
// bridge that ended. It used to be spared the question by hangup_after_bridge,
// which took the caller down with the bot leg — and took the fallback with it,
// for exactly the failures the fallback exists for (the application restarts,
// the process dies, a provider drops). Now the script keeps the caller and
// asks: marked means the conversation reached its end, so hang up; unmarked
// means the bot vanished mid-call, so find the caller a human.
//
// Only the two closes that ARE the deliberate ending carry it — the hangup
// tool and a flow reaching its terminal phase. A transfer must not: if the
// transfer fails, the absence of this mark is what sends the caller to the
// fallback queue instead of dropping them, and that failure is likeliest at
// the moment the mark would be wrong.
func (a *callActions) markFinished(how string) {
	if a.callerChannel == "" {
		return
	}
	// The bot is about to close its leg, and a call an agent placed is
	// written by the human path, which reads the bot's tally from the agent's
	// channel. Before the mark, because it is what the dialplan acts on, and
	// before the leg goes, because this is the last moment the bot's time is
	// known.
	if a.recorder != nil && a.facts.isPlacedByAgent() {
		a.stampUnbackedClaims(a.recorder)
		a.stampBotSec(a.recorder)
	}
	a.stampChannel("aicc_bot_finished", how)
}

// handOnCaller puts the switch's teardown rule back before the caller leaves
// us for a queue: from there an agent's hangup ends the call.
func (a *callActions) handOnCaller() {
	if a.callerChannel == "" {
		return
	}
	if err := a.orchestrator.cfg.Switch.EndCallerWithTheirBridge(a.callerChannel); err != nil {
		a.log.Warn("could not restore the caller's teardown rule", "error", err)
	}
}

func (a *callActions) stampChannel(name, value string) {
	if value == "" {
		return
	}
	if err := a.orchestrator.cfg.Switch.SetVariable(a.callerChannel, name, value); err != nil {
		a.log.Warn("could not stamp the caller's channel", "variable", name, "error", err)
	}
}

func (a *callActions) refusalHint() string {
	if a.session != nil && a.session.cfg.Session.Language == "zh" {
		return "这个队列现在无法接听。请向来电者说明，并主动提出为他们留言。"
	}
	return "That queue cannot take the call right now. Explain that to the caller " +
		"and offer to take a message instead."
}

var errNoCallerChannel = errors.New(
	"the bot leg carried no caller channel header, so there is nothing to transfer")
