// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// A bridge to the bot the switch did not confirm (#303).
//
// An answered AI outbound leg is transferred into an inline bridge to the bot
// gateway, and the bridge's INVITE takes the bot slot reserved for it. When the
// command's reply does not come (esl.ErrTimeout) the switch may be stalled with
// it on its socket, and runs it when it recovers. Giving the slot back at once,
// as a failure did, sent that late INVITE in with no reservation behind it; and
// if the switch never ran the bridge, nothing ended the leg, and the customer
// sat in silence on an answered call.
//
// So the bridge is pending. The reservation is kept long enough to cover the
// command's own wait and bridgeUnconfirmedFor after it (it is extended before
// the command is sent, because the command can outlast its default expiry),
// and then the switch decides:
//
//   - the bot leg appears (its CHANNEL_CREATE, or the customer leg's bridge):
//     the bridge ran, and its INVITE takes the slot as any other does;
//   - the customer hangs up: the reservation goes back with the leg;
//   - neither within the window: the reservation is withdrawn (an INVITE that
//     consumed it means the bridge ran after all), so a bridge the switch runs
//     even later is refused at the bot and never starts a conversation with
//     nobody on it; and the customer leg is hung up with BridgeUnconfirmedCause
//     stamped on it, so the CDR says why. The hangup is settled like any other
//     the platform decides (telephony.EndLeg).

// BridgeUnconfirmedCause is the hangupCause of an answered AI outbound call
// whose bridge to the bot the switch did not confirm and that never reached the
// bot within bridgeUnconfirmedFor. It is this platform's own word, not the
// switch's: the leg is stamped with it (telephony.VarHangupCause) before it is
// hung up.
const BridgeUnconfirmedCause = "BOT_BRIDGE_UNCONFIRMED"

const (
	// bridgeUnconfirmedFor is how long after the bridge command was given up
	// on its bot leg may still appear.
	//
	// The customer has answered and hears nothing all this time, on top of the
	// command's own wait: thirty seconds of silence in all, longer than most
	// people stay on a silent line, so the window rarely ends a call its
	// customer had not already left. A switch that has not run the bridge by
	// then is not one to start a conversation through.
	bridgeUnconfirmedFor = 20 * time.Second
	// bridgeKeepMargin keeps the reservation a little past the window, so it
	// is the window that decides and not the reservation's own expiry.
	bridgeKeepMargin = 5 * time.Second
)

// pendingBridge is one bridge to the bot the switch has not confirmed.
type pendingBridge struct {
	callID      uuid.UUID
	customerLeg string
	botLeg      string
	key         string
	to          string
	// isReserved: the bot slot was held for it when the command was sent, so
	// the reservation's fate says whether the INVITE came.
	isReserved bool
	timer      stopper
}

// keepForBridge holds the leg's bot slot over the bridge command and the window
// after it, before the command is sent. It reports whether a slot is held.
func (s *Service) keepForBridge(key string) bool {
	if s.cfg.Slots == nil {
		return false
	}
	return s.cfg.Slots.Keep(key, esl.CommandTimeout+bridgeUnconfirmedFor+bridgeKeepMargin)
}

// awaitBridge tracks a bridge the switch did not confirm until the switch
// settles it or the window ends.
func (s *Service) awaitBridge(callID uuid.UUID, customerLeg, botLeg, key, to string, isReserved bool) {
	pb := &pendingBridge{
		callID: callID, customerLeg: customerLeg, botLeg: botLeg,
		key: key, to: to, isReserved: isReserved,
	}
	s.mu.Lock()
	if s.bridges == nil {
		s.bridges = map[string]*pendingBridge{}
	}
	s.bridges[customerLeg] = pb
	pb.timer = s.afterFunc(bridgeUnconfirmedFor, func() { s.bridgeExpired(pb) })
	s.mu.Unlock()
}

// settleBridge ends the wait for a pending bridge whose bot leg appeared, or
// whose customer is gone.
func (s *Service) settleBridge(ev telephony.SwitchEvent) {
	s.mu.Lock()
	var settled *pendingBridge
	for leg, pb := range s.bridges {
		isBot := ev.ChannelID == pb.botLeg || ev.OtherChannelID == pb.botLeg
		isBridged := ev.Kind == telephony.KindChannelBridge &&
			(ev.ChannelID == leg || ev.OtherChannelID == leg)
		isGone := ev.Kind == telephony.KindChannelHangup && ev.ChannelID == leg
		if isBot || isBridged || isGone {
			settled = pb
			delete(s.bridges, leg)
			pb.timer.Stop()
			break
		}
	}
	s.mu.Unlock()
	if settled == nil {
		return
	}
	if ev.Kind == telephony.KindChannelHangup && ev.ChannelID == settled.customerLeg {
		s.log.Info("the customer hung up while the bridge to the bot was unconfirmed",
			"callId", settled.callID, "channelId", settled.customerLeg)
		return
	}
	s.log.Info("the switch ran the bridge to the bot late; the call goes on",
		"callId", settled.callID, "channelId", settled.customerLeg, "botLeg", settled.botLeg)
}

// bridgeExpired settles a bridge whose window ended with no bot leg seen.
func (s *Service) bridgeExpired(pb *pendingBridge) {
	s.mu.Lock()
	if s.bridges[pb.customerLeg] != pb {
		s.mu.Unlock()
		return
	}
	delete(s.bridges, pb.customerLeg)
	s.mu.Unlock()

	// The reservation is the bot side's own account: an INVITE that came for
	// it consumed it. Withdrawing it also refuses an INVITE still to come.
	// Without one to tell, the switch is asked whether the bot leg it would
	// have dialled exists, before the key is refused to that leg's INVITE.
	if pb.isReserved {
		if !s.cfg.Slots.Withdraw(pb.key) {
			s.log.Info("the bridge to the bot ran late: its INVITE took the slot",
				"callId", pb.callID, "channelId", pb.customerLeg)
			return
		}
	} else {
		if isUp, err := s.sw.ChannelExists(pb.botLeg); err == nil && isUp {
			s.log.Info("the bridge to the bot ran late: its leg is up",
				"callId", pb.callID, "channelId", pb.customerLeg, "botLeg", pb.botLeg)
			return
		}
		if s.cfg.Slots != nil {
			s.cfg.Slots.Withdraw(pb.key)
		}
	}

	s.log.Warn("the bridge to the bot never reached it; hanging up the answered leg",
		"callId", pb.callID, "channelId", pb.customerLeg, "to", pb.to,
		"waitedSec", int((esl.CommandTimeout + bridgeUnconfirmedFor).Seconds()),
		"cause", BridgeUnconfirmedCause)
	s.dropHold(pb.customerLeg, pb.key)
	s.endLeg(telephony.LegEnding{
		ChannelID:     pb.customerLeg,
		Cause:         "NORMAL_CLEARING",
		PlatformCause: BridgeUnconfirmedCause,
		What:          "a leg whose bridge to the bot never ran",
		Attrs:         []any{"callId", pb.callID},
	})
}

// endLeg ends a leg the platform has decided to end, once, settling a kill the
// switch did not confirm (telephony.EndLeg) on the service's own timers.
func (s *Service) endLeg(e telephony.LegEnding) {
	telephony.EndLeg(s.sw, func(d time.Duration, f func()) { s.afterFunc(d, f) }, e, s.log)
}
