// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// A transfer on its way (#303).
//
// A transfer is a uuid_transfer of the leg that survives it, and a switch that
// stalls reads the command and runs it when it recovers, a minute or two later
// in the incident that found this. Its reply then does not come (esl.ErrTimeout)
// and nothing is known: the leg may yet be moved. A second transfer of the same
// leg sent in that time, by the agent trying again or by the bot, races the
// first when the switch catches up: the caller is moved twice, and the second
// move can pull them out of the queue, or away from the agent who has just
// answered them.
//
// So a leg has at most one transfer on its way. The claim is taken before the
// command is sent, so two requests that race are told apart before either
// reaches the switch, and the second is ErrTransferInFlight (409). The switch
// answering settles it either way: a transfer it confirmed or refused is no
// longer on its way. One it did not answer stays claimed until the leg's own
// events show something moved it (an unbridge or a new bridge, a queue taking
// it, its hangup), or transferInFlightFor has passed.

// ErrTransferInFlight means a transfer of the leg is already on its way and has
// not been settled: the switch has not confirmed it, and the leg's events have
// not shown it moving. A second one is refused rather than sent, because the
// switch may still run the first.
var ErrTransferInFlight = errors.New("a transfer of this leg is already on its way")

// transferInFlightFor bounds how long a transfer the switch did not confirm
// keeps the leg claimed, from the moment its reply was given up on.
//
// The events settle it long before this when the switch runs it: they arrive
// on the same link, the moment it recovers. The bound is for a command the
// switch never runs (the link closed before it was read), where the claim only
// stands between the agent and a retry that is now safe. A minute is six
// command timeouts: longer than any stall that ended in the switch catching up
// without the link being replaced, and short enough that an agent told 409 is
// not kept from passing the caller on for longer than they would wait before
// hanging up instead.
const transferInFlightFor = time.Minute

// transferClaim is one leg's transfer on its way. token tells the request that
// took it from any later one, so it settles only its own claim.
type transferClaim struct {
	token uint64
	until time.Time
}

// transferTokens numbers claims across every call, so a claim moved by a merge
// cannot be mistaken for another call's.
var transferTokens atomic.Uint64

// claimTransfer takes the leg for one transfer, unless one is already on its
// way. A claim past its bound has lapsed and is taken over.
func (c *Call) claimTransfer(channelID string, now time.Time) (uint64, bool) {
	if held, ok := c.transfers[channelID]; ok && now.Before(held.until) {
		return 0, false
	}
	if c.transfers == nil {
		c.transfers = map[string]transferClaim{}
	}
	token := transferTokens.Add(1)
	c.transfers[channelID] = transferClaim{token: token, until: now.Add(transferInFlightFor)}
	return token, true
}

// keepTransfer holds a claim whose transfer the switch did not confirm for the
// bound, counted from now. A claim the leg's events have already settled stays
// settled: the switch ran the transfer while its reply was being waited for.
func (c *Call) keepTransfer(channelID string, token uint64, now time.Time) bool {
	held, ok := c.transfers[channelID]
	if !ok || held.token != token {
		return false
	}
	c.transfers[channelID] = transferClaim{token: token, until: now.Add(transferInFlightFor)}
	return true
}

// releaseTransfer gives up a claim the switch has answered for.
func (c *Call) releaseTransfer(channelID string, token uint64) {
	if held, ok := c.transfers[channelID]; ok && held.token == token {
		delete(c.transfers, channelID)
	}
}

// settleTransfers drops the claims an event shows are no longer on their way:
// the legs it names were moved, or left the bridge the transfer was sent from,
// or ended. Either the transfer ran, or what it was meant to move is gone; a
// new transfer is no longer a second one.
func (c *Call) settleTransfers(ev SwitchEvent) {
	if len(c.transfers) == 0 {
		return
	}
	switch ev.Kind {
	case KindChannelUnbridge, KindChannelBridge, KindChannelHangup, KindQueueMemberJoined:
	default:
		return
	}
	for _, channelID := range []string{ev.ChannelID, ev.OtherChannelID, ev.MemberChannelID} {
		if channelID != "" {
			delete(c.transfers, channelID)
		}
	}
}

// sendTransfer transfers one leg of a call with the leg claimed (above): the
// one path every transfer of a tracked leg takes, the agent's and the bot's.
func (c *Coordinator) sendTransfer(ctx context.Context, callID uuid.UUID, channelID, destination, dialplanContext string) error {
	var (
		token  uint64
		isMine bool
	)
	if err := c.registry.Do(callID, func(call *Call) {
		token, isMine = call.claimTransfer(channelID, c.clock())
	}); err != nil {
		return err
	}
	if !isMine {
		slog.InfoContext(ctx, "transfer refused: one is already on its way for this leg",
			"callId", callID, "channelId", channelID, "destination", destination)
		return ErrTransferInFlight
	}

	err := c.adapter.TransferToExtension(channelID, destination, dialplanContext)
	if errors.Is(err, esl.ErrTimeout) {
		var isKept bool
		_ = c.registry.Do(callID, func(call *Call) {
			isKept = call.keepTransfer(channelID, token, c.clock())
		})
		slog.WarnContext(ctx, "transfer sent and not confirmed: the switch may still run it; "+
			"the leg stays claimed until its events settle it",
			"callId", callID, "channelId", channelID, "destination", destination,
			"isStillClaimed", isKept, "claimedForSec", int(transferInFlightFor.Seconds()), "error", err)
		return err
	}
	_ = c.registry.Do(callID, func(call *Call) { call.releaseTransfer(channelID, token) })
	return err
}

// TransferLeg moves a live leg to a dialplan extension, named by its channel:
// the bot's transfer of its caller, which knows the caller's channel and not
// the call. It has the same one-transfer-at-a-time guard as Transfer, so the
// bot's transfer and any other of the same leg cannot race. A channel no call
// holds is transferred without it: there is nothing to keep it on, and
// refusing would strand the caller on the bot.
func (c *Coordinator) TransferLeg(channelID, extension, dialplanContext string) error {
	callID, ok := c.registry.CallForChannel(channelID)
	if !ok {
		return c.adapter.TransferToExtension(channelID, extension, dialplanContext)
	}
	return c.sendTransfer(context.Background(), callID, channelID, extension, dialplanContext)
}

// clock is the coordinator's time; tests replace it.
func (c *Coordinator) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
