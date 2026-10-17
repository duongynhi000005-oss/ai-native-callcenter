// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/calleesim"
	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/phonenum"
)

// campaignScopes are what the campaign driver's API key may do: provision a
// flow and DIDs, keep the do-not-call list, run campaigns and read the
// ledger. Nothing else.
var campaignScopes = []string{
	"config:read", "config:write", "campaigns:read", "campaigns:write", "dnc:read", "dnc:write", "history:read:all",
}

// CampaignConfig is everything the campaign driver needs to know about its
// environment.
type CampaignConfig struct {
	// API is the application's REST base; ReadyURL its readiness endpoint.
	API, ReadyURL string
	// CalleeSim is the simulator's report base; MockProvider the mock
	// provider's HTTP base, for its session counters.
	CalleeSim, MockProvider string
	// ESLAddr and ESLPassword reach the switch, for the gateway check.
	ESLAddr, ESLPassword string
	// ComposeConfig is the resolved compose configuration (JSON): the guard
	// reads it, and so does the driver, for the deployment's dial form.
	ComposeConfig string
	// ScenarioFile is the simulator's scenario; every case must be in it.
	ScenarioFile string

	Username, Password string
	FlowFile           string

	// OutDir receives report.json, probes.md and bundles/.
	OutDir string
	// Wait bounds each phase: the first attempt of every contact, and every
	// contact settling.
	Wait time.Duration

	// Cases and Regions default to DefaultCases and Regions.
	Cases   []Case
	Regions []Region
	// AfterRun are checked once the run is over; nil means AfterRunChecks.
	AfterRun []AfterRunCheck
}

// RunCampaigns runs the scenarios and probes: one campaign per region, each
// holding every case, both started together. It writes the report to OutDir
// and returns it, with an error when the run was refused, could not proceed,
// or an assertion failed.
func RunCampaigns(ctx context.Context, cfg CampaignConfig) (*CampaignReport, error) {
	if len(cfg.Cases) == 0 {
		cfg.Cases = DefaultCases
	}
	if len(cfg.Regions) == 0 {
		cfg.Regions = Regions
	}
	if cfg.AfterRun == nil {
		cfg.AfterRun = AfterRunChecks
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, err
	}

	// Refuse before anything is sent.
	composeJSON, err := os.ReadFile(cfg.ComposeConfig)
	if err != nil {
		return nil, fmt.Errorf("read compose config: %w", err)
	}
	if err := CheckComposeConfig(composeJSON); err != nil {
		return nil, err
	}
	rawForm, err := appSetting(composeJSON, "AICC_OUTBOUND_DIAL_FORM")
	if err != nil {
		return nil, err
	}
	form, err := phonenum.ParseDialForm(rawForm)
	if err != nil {
		return nil, err
	}
	scenario, err := calleesim.LoadScenario(cfg.ScenarioFile)
	if err != nil {
		return nil, err
	}
	if err := CheckScenarioCoversCases(scenario, cfg.Regions, cfg.Cases, form); err != nil {
		return nil, err
	}
	if err := checkSwitch(ctx, cfg.ESLAddr, cfg.ESLPassword); err != nil {
		return nil, err
	}

	c := &httpc{base: cfg.API, client: newClient()}
	if err := waitReady(ctx, c.client, cfg.ReadyURL, cfg.Wait); err != nil {
		return nil, err
	}
	if err := c.signInWithKey(ctx, cfg.Username, cfg.Password, "e2e-campaign", campaignScopes); err != nil {
		return nil, err
	}

	rep := &CampaignReport{StartedAt: time.Now().UTC(), DialForm: string(form), Latency: map[string]float64{}}
	d := &driver{cfg: cfg, c: c, form: form, rep: rep}
	runErr := d.run(ctx)

	rep.FinishedAt = time.Now().UTC()
	rep.DurationSec = rep.FinishedAt.Sub(rep.StartedAt).Seconds()
	rep.Failures = collectFailures(rep)
	if runErr != nil {
		rep.Failures = append(rep.Failures, runIncomplete+runErr.Error())
	}
	rep.Passed = len(rep.Failures) == 0
	if err := writeBundles(cfg.OutDir, rep); err != nil && runErr == nil {
		runErr = err
	}
	if err := writeJSON(filepath.Join(cfg.OutDir, "report.json"), rep); err != nil && runErr == nil {
		runErr = err
	}
	if err := os.WriteFile(filepath.Join(cfg.OutDir, "probes.md"), []byte(ProbeTable(rep.Probes)), 0o644); err != nil && runErr == nil {
		runErr = err
	}
	if runErr != nil {
		return rep, runErr
	}
	if !rep.Passed {
		return rep, fmt.Errorf("%d assertion(s) failed: %s", len(rep.Failures), strings.Join(rep.Failures, "; "))
	}
	return rep, nil
}

// runIncomplete prefixes the failure of a run that stopped before its
// assertions.
const runIncomplete = "the run did not complete: "

// appSetting is one variable of the aicc service's environment in a resolved
// compose configuration; "" when it is not set.
func appSetting(composeJSON []byte, key string) (string, error) {
	var cfg composeConfig
	if err := json.Unmarshal(composeJSON, &cfg); err != nil {
		return "", fmt.Errorf("compose config is not JSON: %w", err)
	}
	app, ok := cfg.Services["aicc"]
	if !ok {
		return "", fmt.Errorf("no aicc service in the compose configuration")
	}
	if v := app.Environment[key]; v != nil {
		return *v, nil
	}
	return "", nil
}

// CheckScenarioCoversCases refuses a run whose simulator would not play what
// the cases expect: every dialled case's key, in the deployment's dial form,
// must be in the scenario with the case's provisional and final response, and
// a do-not-call case must be absent, so dialling it would also show as an
// unexpected dialog. The keys of all regions must be distinct.
func CheckScenarioCoversCases(s *calleesim.Scenario, regions []Region, cases []Case, form phonenum.DialForm) error {
	var problems []string
	seen := map[string]string{}
	for _, r := range regions {
		for _, cs := range cases {
			key := r.DialKey(cs, form)
			label := r.Code + " " + cs.ID
			if prev, dup := seen[key]; dup {
				problems = append(problems, fmt.Sprintf("%s and %s dial the same string %s", prev, label, key))
			}
			seen[key] = label
			list := s.Numbers[key]
			if cs.IsOnDNC {
				if len(list) > 0 {
					problems = append(problems, fmt.Sprintf("%s (%s) is on the do-not-call list and must not be in the scenario", label, key))
				}
				continue
			}
			if len(list) != 1 {
				problems = append(problems, fmt.Sprintf("%s: the scenario has %d behaviours for %s, want 1", label, len(list), key))
				continue
			}
			b := list[0]
			wantFinal := cs.SIPStatus
			if wantFinal == 0 {
				wantFinal = neverAnsweredStatus
				if b.FinalAfterMs < neverAnsweredAfterMs {
					problems = append(problems, fmt.Sprintf("%s: a never-answered callee must wait %d ms, has %d", label, neverAnsweredAfterMs, b.FinalAfterMs))
				}
			}
			if b.Final() != wantFinal {
				problems = append(problems, fmt.Sprintf("%s: the scenario answers %d, want %d", label, b.Final(), wantFinal))
			}
			if len(b.Provisionals) != 1 || b.Provisionals[0].Status != cs.Provisional || b.Provisionals[0].EarlyMedia != cs.EarlyMedia {
				problems = append(problems, fmt.Sprintf("%s: the scenario's provisionals are %+v, want one %d (early media %v)", label, b.Provisionals, cs.Provisional, cs.EarlyMedia))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the simulator's scenario does not match the campaign cases: %s", strings.Join(problems, "; "))
	}
	return nil
}

// driver is one run.
type driver struct {
	cfg  CampaignConfig
	c    *httpc
	form phonenum.DialForm
	rep  *CampaignReport

	regions []*regionRun
	dnc     []string
	// calleeBase is how many dialogs the simulator held before the run.
	calleeBase      int
	unexpectedBase  int
	providerBefore  mockprovider.Stats
	providerAfter   mockprovider.Stats
	startedAt       time.Time
	lastObservation time.Time
}

// regionRun is one campaign of the run as the driver tracks it.
type regionRun struct {
	region   Region
	did      api.DID
	campaign api.Campaign
	// byNumber maps a contact's canonical number to its case.
	byNumber map[string]Case
	// afterFirst is each contact as first seen with exactly one attempt, that
	// one finished: the contact right after its first attempt.
	afterFirst map[string]api.CampaignContact
	contacts   map[string]api.CampaignContact
	attempts   map[string][]api.CampaignAttempt
	cdrs       []api.CDR
}

func (d *driver) run(ctx context.Context) error {
	var pre calleesim.Report
	if err := d.c.getJSON(ctx, d.cfg.CalleeSim+"/report", &pre); err != nil {
		return fmt.Errorf("simulator report: %w", err)
	}
	d.calleeBase, d.unexpectedBase = len(pre.Dialogs), pre.UnexpectedCount
	var err error
	if d.providerBefore, err = providerStats(ctx, d.c, d.cfg.MockProvider); err != nil {
		return err
	}

	defer d.removeDNC(context.WithoutCancel(ctx))
	usedDIDs := map[string]bool{}
	for _, r := range d.cfg.Regions {
		rr, err := d.provision(ctx, r, usedDIDs)
		if err != nil {
			return fmt.Errorf("%s: %w", r.Code, err)
		}
		d.regions = append(d.regions, rr)
	}

	// Both campaigns start together: the scenarios run concurrently, never
	// one after another (a retry is at least a minute away).
	d.startedAt = time.Now()
	for _, rr := range d.regions {
		if err := d.c.do(ctx, http.MethodPost, "/campaigns/"+rr.campaign.ID.String()+"/start", nil, nil, http.StatusOK); err != nil {
			return fmt.Errorf("%s: start campaign: %w", rr.region.Code, err)
		}
	}

	if err := d.waitUntil(ctx, "every contact's first attempt to finish", d.firstRoundDone); err != nil {
		return err
	}
	d.rep.Latency["startToFirstRoundMs"] = float64(time.Since(d.startedAt).Milliseconds())
	if err := d.waitUntil(ctx, "every contact to settle", d.settled); err != nil {
		return err
	}
	d.rep.Latency["startToSettledMs"] = float64(time.Since(d.startedAt).Milliseconds())

	// A stray late attempt, INVITE or CDR would land in this window.
	if err := sleep(ctx, 5*time.Second); err != nil {
		return err
	}
	if err := d.observe(ctx); err != nil {
		return err
	}
	var post calleesim.Report
	if err := d.c.getJSON(ctx, d.cfg.CalleeSim+"/report", &post); err != nil {
		return fmt.Errorf("simulator report: %w", err)
	}
	for _, rr := range d.regions {
		if rr.cdrs, err = d.campaignCDRs(ctx, rr.campaign.ID.String()); err != nil {
			return err
		}
	}
	if d.providerAfter, err = providerStats(ctx, d.c, d.cfg.MockProvider); err != nil {
		return err
	}

	view := d.view(post)
	assess(d.rep, view, d.cfg.Cases, d.form, d.unexpectedBase)
	for _, chk := range d.cfg.AfterRun {
		var col Checks
		chk.Run(ctx, view, &col)
		d.rep.AfterRun = append(d.rep.AfterRun, AfterRunResult{Name: chk.Name, CheckList: col.list()})
	}
	return nil
}

func (d *driver) provision(ctx context.Context, r Region, usedDIDs map[string]bool) (*regionRun, error) {
	var did api.DID
	for try := 0; ; try++ {
		number := fmt.Sprintf("95%06d", rand.IntN(1_000_000))
		if usedDIDs[number] {
			continue
		}
		var err error
		slug := "e2e-campaign-" + strings.ToLower(r.Code) + "-" + number
		did, err = d.c.publishFlowAndDID(ctx, d.cfg.FlowFile, slug, "e2e campaign "+r.Code+" "+number, number)
		if err == nil {
			usedDIDs[number] = true
			break
		}
		if try == 2 {
			return nil, err
		}
	}

	rr := &regionRun{
		region: r, did: did, byNumber: map[string]Case{},
		afterFirst: map[string]api.CampaignContact{},
	}
	for _, cs := range d.cfg.Cases {
		rr.byNumber[r.Number(cs)] = cs
		if !cs.IsOnDNC {
			continue
		}
		n := r.Number(cs)
		// 200 when an earlier run that was cut short left it listed.
		if err := d.c.do(ctx, http.MethodPost, "/dnc-numbers", api.DNCNumberWrite{Number: n}, nil, http.StatusOK, http.StatusCreated); err != nil {
			return nil, fmt.Errorf("add %s to the do-not-call list: %w", n, err)
		}
		d.dnc = append(d.dnc, n)
	}

	maxConcurrent := len(d.cfg.Cases)
	ring := RingTimeoutSec
	attempts, system := MaxAttempts, MaxSystemRetries
	base, factor, capSec := RetryIntervalSec, 1, RetryIntervalSec
	intervals := api.CampaignIntervalSec{
		"NO_ANSWER": RetryIntervalSec, "BUSY": RetryIntervalSec, "REJECTED": RetryIntervalSec, "UNKNOWN": RetryIntervalSec,
	}
	outcomes := []api.CampaignRetryOutcome{
		api.CampaignRetryOutcomeNOANSWER, api.CampaignRetryOutcomeBUSY, api.CampaignRetryOutcomeREJECTED,
		api.CampaignRetryOutcomeUNKNOWN, api.CampaignRetryOutcomeTRANSIENT, api.CampaignRetryOutcomeORIGINATEFAILED,
	}
	cc := r.CountryCode
	if err := d.c.do(ctx, http.MethodPost, "/campaigns", api.CampaignWrite{
		Name:               "e2e " + r.Code + " " + did.Number,
		DIDID:              did.ID,
		TimeZone:           r.TimeZone,
		CallingWindow:      api.CampaignCallingWindow{Days: allDays, Ranges: []api.CampaignTimeRange{{Start: "00:00", End: "24:00"}}},
		MaxConcurrent:      &maxConcurrent,
		RingTimeoutSec:     &ring,
		DefaultCountryCode: &cc,
		RetryPolicy: &api.CampaignRetryPolicyWrite{
			MaxAttempts: &attempts, MaxSystemRetries: &system, RetryOutcomes: &outcomes, IntervalSec: &intervals,
			TransientBackoff: &api.CampaignBackoffWrite{BaseSec: &base, Factor: &factor, CapSec: &capSec},
		},
	}, &rr.campaign, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("create campaign: %w", err)
	}

	rows := make([]api.CampaignContactRow, 0, len(d.cfg.Cases))
	for _, cs := range d.cfg.Cases {
		rows = append(rows, api.CampaignContactRow{Number: r.Number(cs)})
	}
	var imp api.CampaignImportResult
	if err := d.c.do(ctx, http.MethodPost, "/campaigns/"+rr.campaign.ID.String()+"/contacts", api.CampaignContactsImport{Contacts: rows}, &imp, http.StatusOK); err != nil {
		return nil, fmt.Errorf("import contacts: %w", err)
	}
	if imp.Imported != len(rows) || len(imp.Rows) != len(rows) || imp.Rejected != 0 || imp.Duplicates != 0 {
		return nil, fmt.Errorf("import: %d imported, %d duplicates, %d rejected of %d rows", imp.Imported, imp.Duplicates, imp.Rejected, len(rows))
	}
	return rr, nil
}

var allDays = []api.Weekday{api.WeekdayMON, api.WeekdayTUE, api.WeekdayWED, api.WeekdayTHU, api.WeekdayFRI, api.WeekdaySAT, api.WeekdaySUN}

func (d *driver) removeDNC(ctx context.Context) {
	for _, n := range d.dnc {
		_ = d.c.do(ctx, http.MethodDelete, "/dnc-numbers/"+url.PathEscape(n), nil, nil, http.StatusNoContent, http.StatusNotFound)
	}
}

func providerStats(ctx context.Context, c *httpc, base string) (mockprovider.Stats, error) {
	var s mockprovider.Stats
	if err := c.getJSON(ctx, base+mockprovider.StatsPath, &s); err != nil {
		return s, fmt.Errorf("mock provider stats: %w", err)
	}
	return s, nil
}

// observe reads both campaigns, their contacts and their attempts once, and
// remembers each contact as it stood right after its first attempt.
func (d *driver) observe(ctx context.Context) error {
	for _, rr := range d.regions {
		id := rr.campaign.ID.String()
		var camp api.Campaign
		if err := d.c.do(ctx, http.MethodGet, "/campaigns/"+id, nil, &camp, http.StatusOK); err != nil {
			return err
		}
		// Attempts before contacts: an attempt's finish and its contact's
		// counters commit in one transaction, so a contact read after its
		// attempt was seen FINISHED includes that finish. Read the other way
		// round, a finish landing between the two reads would pair a finished
		// attempt with the contact as it was before.
		var attempts api.CampaignAttemptList
		if err := d.c.do(ctx, http.MethodGet, "/campaigns/"+id+"/attempts?limit=200", nil, &attempts, http.StatusOK); err != nil {
			return err
		}
		var contacts api.CampaignContactList
		if err := d.c.do(ctx, http.MethodGet, "/campaigns/"+id+"/contacts?limit=200", nil, &contacts, http.StatusOK); err != nil {
			return err
		}
		if contacts.Total != len(contacts.Items) || attempts.Total != len(attempts.Items) {
			return fmt.Errorf("%s: a page did not hold every row (contacts %d of %d, attempts %d of %d)",
				rr.region.Code, len(contacts.Items), contacts.Total, len(attempts.Items), attempts.Total)
		}
		rr.campaign = camp
		rr.contacts = map[string]api.CampaignContact{}
		rr.attempts = map[string][]api.CampaignAttempt{}
		for _, ct := range contacts.Items {
			rr.contacts[ct.Number] = ct
		}
		for _, a := range attempts.Items {
			rr.attempts[a.Number] = append(rr.attempts[a.Number], a)
		}
		for n, list := range rr.attempts {
			sort.SliceStable(list, func(i, j int) bool { return list[i].StartedAt.Before(list[j].StartedAt) })
			if _, seen := rr.afterFirst[n]; !seen && len(list) == 1 && list[0].State == attemptFinished {
				if ct, ok := rr.contacts[n]; ok {
					rr.afterFirst[n] = ct
				}
			}
		}
	}
	d.lastObservation = time.Now().UTC()
	return nil
}

const attemptFinished = api.CampaignAttemptState("FINISHED")

func (d *driver) firstRoundDone() bool {
	for _, rr := range d.regions {
		for n := range rr.byNumber {
			if _, ok := rr.afterFirst[n]; !ok {
				return false
			}
		}
	}
	return true
}

func (d *driver) settled() bool {
	for _, rr := range d.regions {
		for n := range rr.byNumber {
			ct, ok := rr.contacts[n]
			if !ok || ct.Status == api.CampaignContactStatus("ACTIVE") {
				return false
			}
			for _, a := range rr.attempts[n] {
				if a.State != attemptFinished {
					return false
				}
			}
		}
	}
	return true
}

func (d *driver) waitUntil(ctx context.Context, what string, done func() bool) error {
	deadline := time.Now().Add(d.cfg.Wait)
	for {
		err := d.observe(ctx)
		if err == nil && done() {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("waiting for %s: %w", what, err)
			}
			return fmt.Errorf("waiting for %s: not within %s", what, d.cfg.Wait)
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
}

func (d *driver) campaignCDRs(ctx context.Context, campaignID string) ([]api.CDR, error) {
	var l api.CDRList
	if err := d.c.do(ctx, http.MethodGet, "/cdrs?limit=200&campaignId="+campaignID, nil, &l, http.StatusOK); err != nil {
		return nil, fmt.Errorf("list cdrs: %w", err)
	}
	if l.Total != len(l.Items) {
		return nil, fmt.Errorf("the CDR page did not hold every row (%d of %d)", len(l.Items), l.Total)
	}
	return l.Items, nil
}

// view is what the run left, for the assessment and the after-run checks.
func (d *driver) view(post calleesim.Report) *RunView {
	v := &RunView{c: d.c, ObservedAt: d.lastObservation, Provider: ProviderCount{
		TotalBefore: d.providerBefore.Total, TotalAfter: d.providerAfter.Total, LiveAfter: d.providerAfter.Live,
	}}
	if d.calleeBase <= len(post.Dialogs) {
		v.Dialogs = post.Dialogs[d.calleeBase:]
	}
	v.Callee = post
	for _, rr := range d.regions {
		rv := RegionView{Region: rr.region, DID: rr.did, Campaign: rr.campaign, CDRs: rr.cdrs}
		for _, cs := range d.cfg.Cases {
			n := rr.region.Number(cs)
			cv := ContactView{Case: cs, Number: n, Attempts: rr.attempts[n]}
			if ct, ok := rr.contacts[n]; ok {
				cv.Contact = &ct
			}
			if ct, ok := rr.afterFirst[n]; ok {
				cv.AfterFirst = &ct
			}
			rv.Contacts = append(rv.Contacts, cv)
		}
		v.Regions = append(v.Regions, rv)
	}
	return v
}
