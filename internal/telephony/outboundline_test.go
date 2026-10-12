// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

func TestOutboundLineLegTable(t *testing.T) {
	t.Parallel()
	base := func(mod func(*SwitchEvent)) SwitchEvent {
		ev := SwitchEvent{
			Direction: DirectionOutbound, LegDID: "95011", DestinationNumber: "13900000001",
			Raw: esl.NewEvent(map[string]string{"variable_aicc_did": "95011"}, ""),
		}
		if mod != nil {
			mod(&ev)
		}
		return ev
	}
	tests := []struct {
		name string
		ev   SwitchEvent
		want bool
	}{
		{"a hand-dialled trunk leg", base(nil), true},
		{"inbound direction", base(func(e *SwitchEvent) { e.Direction = DirectionInbound }), false},
		{"no DID on the leg", base(func(e *SwitchEvent) { e.LegDID = "" }), false},
		{"the leg towards the bot", base(func(e *SwitchEvent) { e.DestinationNumber = "95011" }), false},
		{"a queue delivery", base(func(e *SwitchEvent) { e.MemberChannelID = "x" }), false},
		{"a leg the dialplan raised between extensions", base(func(e *SwitchEvent) { e.ParentChannelID = "x" }), false},
		{"typed inbound", base(func(e *SwitchEvent) { e.CallTypeHint = "INBOUND" }), false},
		{"typed internal", base(func(e *SwitchEvent) { e.CallTypeHint = "INTERNAL" }), false},
		{"typed outbound", base(func(e *SwitchEvent) { e.CallTypeHint = "OUTBOUND" }), true},
		{"the customer leg of an AI call", base(func(e *SwitchEvent) {
			e.Raw = esl.NewEvent(map[string]string{"variable_aicc_did": "95011", "variable_" + VarPlacedBy: PlacedByDialAI}, "")
		}), false},
		{"no raw event", SwitchEvent{Direction: DirectionOutbound, LegDID: "95011"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			did, ok := OutboundLineLeg(tt.ev)
			if ok != tt.want || (ok && did != "95011") {
				t.Errorf("OutboundLineLeg = %q, %v; want %v", did, ok, tt.want)
			}
		})
	}
}
