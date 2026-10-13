// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"regexp"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// An unbacked claim is the bot telling the caller it is doing something, or
// has done it, in a turn where nothing was done (#44). The flow cannot see
// one: its only events are tool results and silence, so "I'm transferring you"
// with no transfer_to_agent behind it moves no phase, arms no transfer and
// leaves the caller waiting for a person who is not coming. Live calls on qwen
// showed it for transfers ("I've already transferred you", ten turns running
// with the queue open), lookups ("我帮您查询一下", eight times with no lookup) and
// goodbyes with nothing ending the call.
//
// Detection reads the bot's own transcript, so it is a signal and not proof.
// The patterns are the claim's own phrasing — a first person doing the thing,
// or the thing reported done — and leave out the shapes that only offer it: a
// question, a condition, a "can" or "would you like". A claim missed here is
// one the conversation already hid; a claim invented here would make the
// count one nobody trusts, so the patterns err narrow.

// claim is one kind of action a bot line claims, and the words that claimed it.
type claim struct {
	kind   string
	phrase string
}

// claimPattern recognises one kind of claim.
type claimPattern struct {
	kind string
	re   *regexp.Regexp
}

// claimPatterns are checked against each sentence of a bot line. The English
// ones see the sentence lowercased with typographic apostrophes straightened.
var claimPatterns = []claimPattern{
	// Transfer: "I'm transferring you", "I've already transferred you", "I've
	// initiated the transfer", "the transfer … is being processed", "let me
	// put you through", "我帮您转接人工", "正在为您转接", "已为您接通".
	{store.UnbackedClaimTransfer, regexp.MustCompile(
		`\bi(?:'m| am) (?:now |just )?(?:transferring|connecting|putting) you\b` +
			`|\bi(?:'ve| have) (?:(?:now|just|already) )*(?:transferred|connected|put) you\b` +
			`|\bi(?:'ve| have) (?:(?:now|just|already) )*(?:initiated|started|made|requested|placed) (?:the|a|your) transfer\b` +
			`|\b(?:i(?:'ll| will|'m going to| am going to)|let me)(?: now| just)? (?:transfer|connect|put) you\b` +
			`|\bthe transfer\b.{0,40}\b(?:is being processed|is in progress|is underway|has been (?:initiated|started|made|completed|processed))` +
			`|\byou(?:'re| are) (?:now )?being (?:transferred|connected|put through)\b` +
			`|\byou(?:'ll| will) (?:now )?be (?:transferred|connected|put through)\b` +
			`|(?:我|这就|马上|现在|正在|立即|立刻|已经?)(?:来|就|会)?(?:为|给|帮)您(?:转接|转到|转给|接通|转人工)` +
			`|(?:正在|马上|立即|立刻|已经?)(?:转接|接通)` +
			`|(?:为|给)您转接中`)},
	// Lookup: "let me check", "I'm looking that up", "我帮您查询一下",
	// "正在为您核实".
	{store.UnbackedClaimLookup, regexp.MustCompile(
		`\bi(?:'m| am) (?:now |just )?(?:checking|looking (?:that |this |it )?up|looking into|pulling up|searching)\b` +
			`|\b(?:let me|i(?:'ll| will))(?: now| just| quickly)? (?:check|look (?:that |this |it )?up|look into|pull up|search)\b` +
			`|(?:我|这就|马上|现在|正在|立即)(?:来|就|会)?(?:为|给|帮)您(?:查询|查一下|查查|查找|核实|核查|查看)` +
			`|(?:正在|马上)(?:为您)?(?:查询|查找|核实|核查)` +
			`|我(?:来|先)?查(?:询)?一下`)},
	// Message: only the completed form. "I'll take your message" usually comes
	// before the message has been said, and the tool call rightly after it.
	{store.UnbackedClaimMessage, regexp.MustCompile(
		`\bi(?:'ve| have) (?:(?:now|just|already) )*(?:saved|recorded|noted down|logged|taken|passed on|submitted|forwarded) (?:your|the) (?:message|callback|request|details|number)\b` +
			`|\byour (?:message|callback request|request) (?:has been|is) (?:saved|recorded|logged|passed on|submitted|forwarded)\b` +
			`|已(?:经)?(?:为|帮|给)?您(?:记录|登记|提交|转达|留言)` +
			`|(?:留言|信息|请求|回电请求)已(?:经)?(?:记录|登记|提交|转达)`)},
	// Farewell: a goodbye is a claim that the call is ending.
	// Besides the word itself, the closing phrases the seeded flows use ("Thanks
	// for your time", "祝您生活愉快"). A plain "thank you" is not one: it is said
	// mid-call all the time.
	{store.UnbackedClaimFarewell, regexp.MustCompile(
		`(?i)\b(?:good-?bye|bye-bye|bye now)\b|` +
			`\bthank(?:s| you) for your (?:time|cooperation)\b|` +
			`\bhave a (?:great|good|nice|wonderful|lovely) (?:day|evening|night)\b|` +
			`\btake care(?:[.!,]|\s*$)|` +
			`再见|拜拜|祝您[^，。！？,.!?]{0,8}愉快|感谢您的(?:配合|时间)|感谢配合`)},
}

// notAClaim marks a sentence that offers or supposes rather than claims.
var notAClaim = regexp.MustCompile(`\bif\b|\bwhether\b|如果|的话|是否|要不要|吗`)

// detectClaims returns the actions a bot line claims, each kind once.
func detectClaims(text string) []claim {
	var claims []claim
	seen := map[string]bool{}
	for _, sentence := range sentences(text) {
		if strings.HasSuffix(sentence, "?") || strings.HasSuffix(sentence, "？") {
			continue
		}
		normalized := strings.ToLower(strings.ReplaceAll(sentence, "’", "'"))
		if notAClaim.MatchString(normalized) {
			continue
		}
		for _, pattern := range claimPatterns {
			if seen[pattern.kind] {
				continue
			}
			if phrase := pattern.re.FindString(normalized); phrase != "" {
				seen[pattern.kind] = true
				claims = append(claims, claim{kind: pattern.kind, phrase: phrase})
			}
		}
	}
	return claims
}

// sentences splits a line after each terminator, keeping it, so a question
// can be told from a statement.
func sentences(text string) []string {
	var out []string
	start := 0
	for i, r := range text {
		switch r {
		case '.', '!', '?', '。', '！', '？', '\n':
			end := i + len(string(r))
			if s := strings.TrimSpace(text[start:end]); s != "" {
				out = append(out, s)
			}
			start = end
		}
	}
	if s := strings.TrimSpace(text[start:]); s != "" {
		out = append(out, s)
	}
	return out
}

// claimWatch follows the bot's lines turn by turn, on the call's own
// goroutine, and reports the claims no tool call backs.
//
// A turn is judged when it is done, not when its words arrive: a response can
// speak first and call the tool after, and the transcript of the words lands
// before the call does. A claim is backed when its turn made a tool call, when
// the turn is the reply to one (the bot saying "I'm transferring you now"
// after transfer_to_agent returned is the truth), or when an ending is already
// armed (a goodbye the platform asked for, or a line before a transfer the
// flow decided).
type claimWatch struct {
	// pending is the text of turns not yet done.
	pending map[int]string
	// lastDoneTurn and isLastDoneBacked judge text that arrives after its own
	// turn finished; 0 is none, turns are numbered from 1.
	lastDoneTurn     int
	isLastDoneBacked bool
	// isToolReplyNext says the next turn to finish answers a tool result.
	isToolReplyNext bool
	// isFarewellUnhung says the latest finished turn said goodbye in words
	// with nothing behind it: no tool call, no armed ending (#147). The next
	// turn of any kind, or a tool call, takes it back.
	isFarewellUnhung bool
}

// hasFarewellWithoutHangup reports whether the bot's last finished turn said
// goodbye and did nothing to end the call. A caller who then goes quiet is
// not waiting for a question; the conversation is over and nobody ended it.
func (w *claimWatch) hasFarewellWithoutHangup() bool { return w.isFarewellUnhung }

// onToolCall takes the farewell back: the model is acting after all.
func (w *claimWatch) onToolCall() { w.isFarewellUnhung = false }

// onTurnInterrupted takes the farewell back: a turn the caller talked over was
// not heard to the end, so nothing was said that the call could be left on.
func (w *claimWatch) onTurnInterrupted() { w.isFarewellUnhung = false }

// consumeFarewell takes the farewell back once it has been acted on.
func (w *claimWatch) consumeFarewell() { w.isFarewellUnhung = false }

// hasFarewell reports whether any of the claims is a goodbye.
func hasFarewell(found []claim) bool {
	for _, c := range found {
		if c.kind == store.UnbackedClaimFarewell {
			return true
		}
	}
	return false
}

// onBotSaid takes a final line of the bot's. A line for the turn that just
// finished is judged at once; one for an older turn is let go, since what
// backed it can no longer be known.
func (w *claimWatch) onBotSaid(turn int, text string) []claim {
	if text == "" {
		return nil
	}
	switch {
	case w.lastDoneTurn != 0 && turn == w.lastDoneTurn:
		if w.isLastDoneBacked {
			return nil
		}
		found := detectClaims(text)
		w.isFarewellUnhung = w.isFarewellUnhung || hasFarewell(found)
		return found
	case turn < w.lastDoneTurn:
		return nil
	}
	if w.pending == nil {
		w.pending = map[int]string{}
	}
	if prior := w.pending[turn]; prior != "" {
		text = prior + " " + text
	}
	w.pending[turn] = text
	return nil
}

// onTurnDone judges a finished turn's lines and returns its unbacked claims.
func (w *claimWatch) onTurnDone(turn int, isToolCall, isArmed bool) []claim {
	isBacked := isToolCall || w.isToolReplyNext || isArmed
	w.isToolReplyNext = isToolCall
	w.lastDoneTurn = turn
	w.isLastDoneBacked = isBacked
	w.isFarewellUnhung = false

	text := w.pending[turn]
	for pendingTurn := range w.pending {
		if pendingTurn <= turn {
			delete(w.pending, pendingTurn)
		}
	}
	if isBacked || text == "" {
		return nil
	}
	found := detectClaims(text)
	w.isFarewellUnhung = hasFarewell(found)
	return found
}
