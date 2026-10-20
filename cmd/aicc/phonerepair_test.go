// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/testdb"
)

// The startup repair (#268) against a real database: the rows it repairs are
// the ones the old provisioning left, and the allocation is the pool's own.

func repairStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, testdb.ScratchDSN(t, "phonerepair"), 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func provision(t *testing.T, st *store.Store, username, displayName, role string) store.Account {
	t.Helper()
	got, err := st.Accounts().Provision(context.Background(), store.NewAccount{
		Username: username, PasswordHash: "not-a-hash", DisplayName: displayName, Role: role,
		CallcenterName: "agent-" + username, SIPPassword: "sip-" + username,
		ExtensionLow: 1000, ExtensionHigh: 1999,
	})
	if err != nil {
		t.Fatalf("provision %s: %v", username, err)
	}
	return got
}

// takePhone leaves an account the way the old rows look: identity, no phone,
// the extension it once had gone. It returns the number it had.
func takePhone(t *testing.T, st *store.Store, a store.Account) string {
	t.Helper()
	ctx := context.Background()
	var phoneID uuid.UUID
	if err := st.Pool.QueryRow(ctx, `SELECT default_extension_id FROM agents WHERE id = $1`,
		*a.AgentID).Scan(&phoneID); err != nil {
		t.Fatalf("read the phone of %s: %v", a.Username, err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE agents SET default_extension_id = NULL WHERE id = $1`, *a.AgentID); err != nil {
		t.Fatalf("unbind %s: %v", a.Username, err)
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM extensions WHERE id = $1`, phoneID); err != nil {
		t.Fatalf("delete the phone of %s: %v", a.Username, err)
	}
	return a.ExtensionNumber
}

func setUser(t *testing.T, st *store.Store, a store.Account, column, value string) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		`UPDATE users SET `+column+` = $2 WHERE id = $1`, a.UserID, value); err != nil {
		t.Fatalf("set %s of %s: %v", column, a.Username, err)
	}
}

type accountRow struct {
	agentID   string
	extension string
	role      string
	status    string
}

func snapshot(t *testing.T, st *store.Store) map[string]accountRow {
	t.Helper()
	rows, err := st.Accounts().List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := make(map[string]accountRow, len(rows))
	for _, r := range rows {
		row := accountRow{extension: r.ExtensionNumber, role: r.Role, status: r.Status}
		if r.AgentID != nil {
			row.agentID = r.AgentID.String()
		}
		out[r.Username] = row
	}
	return out
}

// Fifty call-taking accounts, twelve of them without a phone: both roles, one
// suspended, one whose old number somebody else was given since. Afterwards
// every call-taker has a phone of their own, nobody else changed, and a second
// start finds nothing to do.
func TestTheStartupRepairGivesEveryPhonelessCallTakerAPhone(t *testing.T) {
	st := repairStore(t)
	ctx := context.Background()

	// Messy on purpose: names in two scripts, punctuation the username allows.
	names := []string{"wang.wei", "li-na", "o_connor", "zhang.san", "amy", "cara", "ben", "j.smith"}
	display := []string{"王伟", "李娜", "Siobhán O'Connor", "张三", "Amy", "Cara", "Ben", "J. Smith"}
	var callTakers []store.Account
	for i := range 49 {
		role := "AGENT"
		if i%5 == 0 {
			role = "SUPERVISOR"
		}
		u := fmt.Sprintf("%s%02d", names[i%len(names)], i)
		callTakers = append(callTakers, provision(t, st, u, display[i%len(display)], role))
	}
	// Twelve lose their phones; the first of them has the lowest number in
	// the set, so the newcomer below is handed exactly that number.
	phoneless := []int{3, 4, 7, 10, 15, 18, 22, 25, 30, 37, 41, 48}
	oldNumbers := map[string]string{}
	for _, i := range phoneless {
		oldNumbers[callTakers[i].Username] = takePhone(t, st, callTakers[i])
	}
	// Somebody hired since got the reused number.
	newcomer := provision(t, st, "newcomer", "New Comer", "AGENT")
	if newcomer.ExtensionNumber != oldNumbers[callTakers[3].Username] {
		t.Fatalf("newcomer got %s, want the reused %s", newcomer.ExtensionNumber, oldNumbers[callTakers[3].Username])
	}
	// The reused number's former owner still has it on their logged-out
	// presence row, as a row from before the repair might.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO agent_states (agent_id, state, extension_number)
		VALUES ($1, 'LOGGED_OUT', $2)`, *callTakers[3].AgentID, newcomer.ExtensionNumber); err != nil {
		t.Fatalf("seed presence: %v", err)
	}
	setUser(t, st, callTakers[22], "status", "SUSPENDED")

	// Administrators: two who never took calls, one who used to and kept a
	// phoneless identity when demoted, and one demoted with a phone.
	provision(t, st, "root", "Root", "ADMIN")
	provision(t, st, "ops", "运维", "ADMIN")
	demoted := provision(t, st, "former.agent", "Former Agent", "AGENT")
	takePhone(t, st, demoted)
	setUser(t, st, demoted, "role", "ADMIN")
	demotedWithPhone := provision(t, st, "former.sup", "Former Supervisor", "SUPERVISOR")
	setUser(t, st, demotedWithPhone, "role", "ADMIN")

	before := snapshot(t, st)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	repaired := repairPhonelessAgents(ctx, st.Accounts(), 1000, 1999, log)
	if len(repaired) != len(phoneless) {
		t.Errorf("repaired %d accounts, want %d", len(repaired), len(phoneless))
	}

	after := snapshot(t, st)
	taken := map[string]string{}
	callTaking := 0
	for username, row := range after {
		was := before[username]
		if row.agentID != was.agentID {
			t.Errorf("%s: agent id %q → %q", username, was.agentID, row.agentID)
		}
		if row.role != was.role || row.status != was.status {
			t.Errorf("%s: %s/%s → %s/%s", username, was.role, was.status, row.role, row.status)
		}
		if row.role == "ADMIN" {
			if row.extension != was.extension {
				t.Errorf("administrator %s: phone %q → %q", username, was.extension, row.extension)
			}
			continue
		}
		callTaking++
		if row.extension == "" {
			t.Errorf("%s (%s, %s) still has no phone", username, row.role, row.status)
			continue
		}
		if was.extension != "" && row.extension != was.extension {
			t.Errorf("%s: phone %s → %s; a phone that was there is left alone", username, was.extension, row.extension)
		}
		if other, dup := taken[row.extension]; dup {
			t.Errorf("%s and %s share phone %s", username, other, row.extension)
		}
		taken[row.extension] = username
	}
	if callTaking != 50 {
		t.Errorf("%d call-taking accounts, want 50", callTaking)
	}
	if after["newcomer"].extension != newcomer.ExtensionNumber {
		t.Errorf("newcomer's phone %s → %s", newcomer.ExtensionNumber, after["newcomer"].extension)
	}
	if after[callTakers[22].Username].extension == "" {
		t.Error("the suspended account was not repaired")
	}
	if after["former.agent"].extension != "" {
		t.Error("a demoted administrator's leftover identity was given a phone")
	}
	for _, a := range repaired {
		if !strings.Contains(logs.String(), "username="+a.Username) {
			t.Errorf("the log does not say %s was repaired", a.Username)
		}
	}

	// A second start changes nothing.
	again := repairPhonelessAgents(ctx, st.Accounts(), 1000, 1999, log)
	if len(again) != 0 {
		t.Errorf("second run repaired %d accounts, want none", len(again))
	}
	if second := snapshot(t, st); fmt.Sprint(second) != fmt.Sprint(after) {
		t.Errorf("second run changed the accounts:\n%v\n→\n%v", after, second)
	}
}

// A pool with nothing left is said loudly and does not stop the server.
func TestTheStartupRepairLogsAnExhaustedPoolAndCarriesOn(t *testing.T) {
	st := repairStore(t)
	ctx := context.Background()

	wei := provision(t, st, "wei", "Wei", "AGENT") // takes 1000, the whole pool below
	amy := provision(t, st, "amy", "Amy", "AGENT")
	takePhone(t, st, amy)
	cara := provision(t, st, "cara", "Cara", "SUPERVISOR")
	takePhone(t, st, cara)

	var logs bytes.Buffer
	repaired := repairPhonelessAgents(ctx, st.Accounts(), 1000, 1000,
		slog.New(slog.NewTextHandler(&logs, nil)))
	if len(repaired) != 0 {
		t.Errorf("repaired %v from an exhausted pool", repaired)
	}
	if !strings.Contains(logs.String(), "AICC_EXTENSION_RANGE") || !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("the exhausted pool was not logged as an error naming the setting:\n%s", logs.String())
	}
	got := snapshot(t, st)
	if got["amy"].extension != "" || got["cara"].extension != "" || got["wei"].extension != wei.ExtensionNumber {
		t.Errorf("after an exhausted repair: %v", got)
	}
}

// The repaired agents are told to the switch once, on the first connect.
func TestRepairedAgentsAreMirroredOnTheFirstConnectOnly(t *testing.T) {
	a, b := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	var told []uuid.UUID
	hook := mirrorOnFirstConnect([]store.Account{{AgentID: &a}, {AgentID: &b}},
		func(_ context.Context, id uuid.UUID) { told = append(told, id) })
	hook(context.Background())
	hook(context.Background())
	if len(told) != 2 || told[0] != a || told[1] != b {
		t.Errorf("mirrored %v, want %v and %v once each", told, a, b)
	}

	var none int
	mirrorOnFirstConnect(nil, func(context.Context, uuid.UUID) { none++ })(context.Background())
	if none != 0 {
		t.Errorf("mirrored %d agents with nothing repaired", none)
	}
}
