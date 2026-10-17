// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
)

// AfterRunChecks run after every campaign run, in order, over everything the
// run left (RunView). Their assertions go into the run's report under
// afterRun, and a failed one fails the run.
//
// This is the plug-in point for checks that hold for any run rather than for
// one scenario: issue #246 adds its global invariants here, one AfterRunCheck
// each. Empty today.
var AfterRunChecks []AfterRunCheck

// AfterRunCheck is one check made on a finished run.
type AfterRunCheck struct {
	Name string
	Run  func(ctx context.Context, run *RunView, checks *Checks)
}

// Checks collects assertions: every one made, and the ones that failed. Pair
// every equality with a cardinality, so that an empty result fails.
type Checks struct {
	checks, failures []string
}

// That records one assertion.
func (c *Checks) That(ok bool, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.checks = append(c.checks, msg)
	if !ok {
		c.failures = append(c.failures, msg)
	}
}

// Failed reports whether any assertion failed.
func (c *Checks) Failed() bool { return len(c.failures) > 0 }

func (c *Checks) list() CheckList {
	return CheckList{Checks: nonNil(c.checks), Failures: nonNil(c.failures)}
}

// RunView is what a finished run left.
type RunView struct {
	c *httpc
	// ObservedAt is when the campaigns were last read.
	ObservedAt time.Time
	Regions    []RegionView
	// Dialogs are the simulator's dialogs that began during the run, in
	// order; Callee is its whole record.
	Dialogs []calleesim.DialogRecord
	Callee  calleesim.Report
	// Provider is the mock provider's session counters around the run.
	Provider ProviderCount
}

// Do calls the public API with the run's scoped key.
func (v *RunView) Do(ctx context.Context, method, path string, in, out any, okStatus ...int) error {
	return v.c.do(ctx, method, path, in, out, okStatus...)
}

// RegionView is one campaign at the end of the run.
type RegionView struct {
	Region   Region
	DID      api.DID
	Campaign api.Campaign
	Contacts []ContactView
	// CDRs are the ledger rows of the campaign.
	CDRs []api.CDR
}

// ContactView is one contact at the end of the run.
type ContactView struct {
	Case   Case
	Number string
	// Contact is the contact at the end; AfterFirst as it stood right after
	// its first attempt finished. Either is nil when it was never seen.
	Contact    *api.CampaignContact
	AfterFirst *api.CampaignContact
	// Attempts are oldest first.
	Attempts []api.CampaignAttempt
}

// ProviderCount is the mock provider's sessions, read before and after.
type ProviderCount struct {
	TotalBefore int64 `json:"totalBefore"`
	TotalAfter  int64 `json:"totalAfter"`
	LiveAfter   int64 `json:"liveAfter"`
}
