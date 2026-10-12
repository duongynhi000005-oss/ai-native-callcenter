// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"context"
	"fmt"
	"slices"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// scriptedActions stands in for the application's call control. It records
// what the flow asked the call to do and answers as the scenario scripts.
type scriptedActions struct {
	sc       Scenario
	transfer *flow.TransferRequest
	// last is the name of the last built-in that succeeded.
	last string
}

func newActions(sc Scenario) *scriptedActions { return &scriptedActions{sc: sc} }

func (a *scriptedActions) ending() string {
	if a.last == "" {
		return "none"
	}
	return a.last
}

func (a *scriptedActions) answer(name string) flow.Result {
	script, ok := a.sc.Actions[name]
	if !ok {
		a.last = name
		return flow.Succeeded(nil, "")
	}
	if script.IsOK != nil && !*script.IsOK {
		return flow.Result{Error: script.Error, Fields: script.Fields, Hint: script.Hint}
	}
	a.last = name
	return flow.Succeeded(script.Fields, script.Hint)
}

func (a *scriptedActions) TransferToAgent(_ context.Context, req flow.TransferRequest) (flow.Result, error) {
	if len(a.sc.Queues) > 0 && !slices.Contains(a.sc.Queues, req.Queue) {
		return flow.Failed(fmt.Sprintf("there is no queue called %q", req.Queue), ""), nil
	}
	result := a.answer(flow.ToolTransferToAgent)
	if result.IsOK {
		a.transfer = &req
	}
	return result, nil
}

func (a *scriptedActions) TakeMessage(context.Context, flow.MessageRequest) (flow.Result, error) {
	return a.answer(flow.ToolTakeMessage), nil
}

func (a *scriptedActions) Hangup(context.Context, flow.HangupRequest) (flow.Result, error) {
	return a.answer(flow.ToolHangup), nil
}
