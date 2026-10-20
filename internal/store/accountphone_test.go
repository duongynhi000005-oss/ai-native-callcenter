// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
)

// A call-taking account whose identity has no phone (#268): accounts from
// before a phone came with every account. Their agent cannot sign in, because
// sign-in happens only at the bound extension, and until this nothing could
// give them one.

// phonelessAccount provisions an account and then takes its phone away the way
// the old rows look: the identity is there, default_extension_id is NULL and
// the extension it once had is gone. It returns the account and its agent id.
func phonelessAccount(t *testing.T, acc *AccountStore, username, role string) (Account, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	in := newAccount(username, role)
	if role == "ADMIN" {
		// An administrator who used to be an agent: provisioned as one, then
		// demoted, which keeps the identity.
		in.Role = "AGENT"
	}
	got, err := acc.Provision(ctx, in)
	if err != nil {
		t.Fatalf("provision %s: %v", username, err)
	}
	agentID := *got.AgentID
	var phoneID uuid.UUID
	if err := acc.pool.QueryRow(ctx,
		`SELECT default_extension_id FROM agents WHERE id = $1`, agentID).Scan(&phoneID); err != nil {
		t.Fatalf("read the phone of %s: %v", username, err)
	}
	if _, err := acc.pool.Exec(ctx,
		`UPDATE agents SET default_extension_id = NULL WHERE id = $1`, agentID); err != nil {
		t.Fatalf("unbind %s: %v", username, err)
	}
	if _, err := acc.pool.Exec(ctx, `DELETE FROM extensions WHERE id = $1`, phoneID); err != nil {
		t.Fatalf("delete the phone of %s: %v", username, err)
	}
	if _, err := acc.pool.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, got.UserID, role); err != nil {
		t.Fatalf("set role of %s: %v", username, err)
	}
	after, err := acc.Get(ctx, got.UserID)
	if err != nil {
		t.Fatalf("read %s: %v", username, err)
	}
	if after.ExtensionNumber != "" || after.AgentID == nil {
		t.Fatalf("%s: agent=%v phone=%q, want an identity and no phone", username, after.AgentID, after.ExtensionNumber)
	}
	return after, agentID
}

// recordHistory files a state-log row and a wrap-up against an agent, the two
// records that hang off the identity's id.
func recordHistory(t *testing.T, acc *AccountStore, agentID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := acc.pool.Exec(ctx, `
		INSERT INTO agent_state_logs (agent_id, state, entered_at, exited_at)
		VALUES ($1, 'READY', now() - interval '2 hours', now() - interval '1 hour')`, agentID); err != nil {
		t.Fatalf("seed state history: %v", err)
	}
	if _, err := acc.pool.Exec(ctx, `
		INSERT INTO wrap_ups (call_id, agent_id, disposition_code, note)
		VALUES ($1, $2, 'RESOLVED', 'called back about the invoice')`, uuid.Must(uuid.NewV7()), agentID); err != nil {
		t.Fatalf("seed a wrap-up: %v", err)
	}
}

func historyOf(t *testing.T, acc *AccountStore, agentID uuid.UUID) (logs, wrapUps int) {
	t.Helper()
	ctx := context.Background()
	if err := acc.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_state_logs WHERE agent_id = $1`, agentID).Scan(&logs); err != nil {
		t.Fatalf("count state history: %v", err)
	}
	if err := acc.pool.QueryRow(ctx,
		`SELECT count(*) FROM wrap_ups WHERE agent_id = $1`, agentID).Scan(&wrapUps); err != nil {
		t.Fatalf("count wrap-ups: %v", err)
	}
	return logs, wrapUps
}

func countExtensions(t *testing.T, acc *AccountStore) int {
	t.Helper()
	var n int
	if err := acc.pool.QueryRow(context.Background(), `SELECT count(*) FROM extensions`).Scan(&n); err != nil {
		t.Fatalf("count extensions: %v", err)
	}
	return n
}

// The phone goes to the identity the account already has. A new identity
// would leave the state history and the wrap-ups attached to one nobody is.
func TestACallTakerWithoutAPhoneGetsOneOnTheIdentityTheyHave(t *testing.T) {
	for _, role := range []string{"AGENT", "SUPERVISOR"} {
		t.Run(role, func(t *testing.T) {
			acc := accountStore(t)
			ctx := context.Background()

			before, agentID := phonelessAccount(t, acc, "amy", role)
			recordHistory(t, acc, agentID)

			got, err := acc.EnsureAgentIdentity(ctx, before.UserID, newAccount("amy", role))
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			if got.ExtensionNumber == "" {
				t.Fatal("no phone was given")
			}
			if got.AgentID == nil || *got.AgentID != agentID {
				t.Errorf("agent id %v → %v; the history hangs off the old one", agentID, got.AgentID)
			}
			if got.CallcenterName != before.CallcenterName {
				t.Errorf("switch name %q → %q; it is never renamed", before.CallcenterName, got.CallcenterName)
			}
			if logs, wrapUps := historyOf(t, acc, agentID); logs != 1 || wrapUps != 1 {
				t.Errorf("history after the phone: %d state rows, %d wrap-ups, want 1 and 1", logs, wrapUps)
			}
			var isEnabled bool
			var display string
			if err := acc.pool.QueryRow(ctx, `
				SELECT e.is_enabled, e.display_name FROM agents a
				JOIN extensions e ON e.id = a.default_extension_id WHERE a.id = $1`,
				agentID).Scan(&isEnabled, &display); err != nil {
				t.Fatalf("read the new phone: %v", err)
			}
			if !isEnabled || display != "amy" {
				t.Errorf("new phone enabled=%v named %q, want enabled and named after the person", isEnabled, display)
			}

			// Asking again is not a second phone.
			again, err := acc.EnsureAgentIdentity(ctx, before.UserID, newAccount("amy", role))
			if err != nil {
				t.Fatalf("ensure again: %v", err)
			}
			if again.ExtensionNumber != got.ExtensionNumber || countExtensions(t, acc) != 1 {
				t.Errorf("second ensure: phone %s → %s, %d extensions",
					got.ExtensionNumber, again.ExtensionNumber, countExtensions(t, acc))
			}
		})
	}
}

// An administrator takes no calls. One who kept an identity from before a
// demotion is not given a phone for it: that would put them back on the
// switch as somebody a queue could offer a call to.
func TestAnAdministratorsLeftoverIdentityGetsNoPhone(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	boss, agentID := phonelessAccount(t, acc, "boss", "ADMIN")
	got, err := acc.EnsureAgentIdentity(ctx, boss.UserID, newAccount("boss", "SUPERVISOR"))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got.ExtensionNumber != "" || got.AgentID == nil || *got.AgentID != agentID {
		t.Errorf("administrator: agent=%v phone=%q, want the old identity untouched and no phone",
			got.AgentID, got.ExtensionNumber)
	}
	if n := countExtensions(t, acc); n != 0 {
		t.Errorf("%d extensions exist, want none", n)
	}

	// Nor one who never had an identity.
	admin, err := acc.Provision(ctx, newAccount("root", "ADMIN"))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	got, err = acc.EnsureAgentIdentity(ctx, admin.UserID, newAccount("root", "SUPERVISOR"))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got.AgentID != nil || got.ExtensionNumber != "" {
		t.Errorf("administrator got agent=%v phone=%q", got.AgentID, got.ExtensionNumber)
	}
}

// An account that has a phone keeps it.
func TestAnAccountWithAPhoneKeepsIt(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	wei, err := acc.Provision(ctx, newAccount("wei", "AGENT"))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	got, err := acc.EnsureAgentIdentity(ctx, wei.UserID, newAccount("wei", "AGENT"))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got.ExtensionNumber != wei.ExtensionNumber || *got.AgentID != *wei.AgentID {
		t.Errorf("phone %s → %s, agent %v → %v", wei.ExtensionNumber, got.ExtensionNumber,
			*wei.AgentID, *got.AgentID)
	}
	if n := countExtensions(t, acc); n != 1 {
		t.Errorf("%d extensions, want 1", n)
	}
}

// A pool with nothing left refuses with the error that says so, and writes
// nothing: no extension, no binding.
func TestAnExhaustedPoolGivesNoPhoneAndWritesNothing(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	amy, agentID := phonelessAccount(t, acc, "amy", "AGENT")
	// The whole two-number pool is taken.
	in := newAccount("wei", "AGENT")
	in.ExtensionLow, in.ExtensionHigh = 1000, 1001
	if _, err := acc.Provision(ctx, in); err != nil {
		t.Fatalf("provision wei: %v", err)
	}
	in = newAccount("ben", "AGENT")
	in.ExtensionLow, in.ExtensionHigh = 1000, 1001
	if _, err := acc.Provision(ctx, in); err != nil {
		t.Fatalf("provision ben: %v", err)
	}

	ask := newAccount("amy", "AGENT")
	ask.ExtensionLow, ask.ExtensionHigh = 1000, 1001
	if _, err := acc.EnsureAgentIdentity(ctx, amy.UserID, ask); !errors.Is(err, catalog.ErrPoolExhausted) {
		t.Fatalf("error = %v, want catalog.ErrPoolExhausted", err)
	}
	after, err := acc.Get(ctx, amy.UserID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after.ExtensionNumber != "" || *after.AgentID != agentID {
		t.Errorf("after the refusal: agent=%v phone=%q", after.AgentID, after.ExtensionNumber)
	}
	if n := countExtensions(t, acc); n != 2 {
		t.Errorf("%d extensions, want the 2 that were there", n)
	}
}

// Two edits of one phoneless account at the same moment give it one phone.
// Both read "no phone" before either writes; without the account lock each
// would allocate, and the second either takes a second number or replaces the
// first's binding and leaves a phone nobody is bound to.
func TestTwoConcurrentEditsOfAPhonelessAccountAllocateOnePhone(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	for round := range 5 {
		amy, agentID := phonelessAccount(t, acc, fmt.Sprintf("amy%d", round), "AGENT")
		before := countExtensions(t, acc)

		// More editors than the store has connections (accountStore opens
		// four): whoever holds the account's lock must finish on the
		// connection it has, or the waiters, each holding one, starve it.
		const editors = 8
		var (
			wg     sync.WaitGroup
			start  = make(chan struct{})
			phones = make([]string, editors)
			errs   = make([]error, editors)
		)
		for i := range editors {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got, err := acc.EnsureAgentIdentity(ctx, amy.UserID, newAccount(amy.Username, "AGENT"))
				phones[i], errs[i] = got.ExtensionNumber, err
			}()
		}
		close(start)
		wg.Wait()

		for i := range editors {
			if errs[i] != nil {
				t.Fatalf("round %d editor %d: %v", round, i, errs[i])
			}
			if phones[i] == "" || phones[i] != phones[0] {
				t.Errorf("round %d: editors saw phones %v, want one and the same", round, phones)
				break
			}
		}
		if got := countExtensions(t, acc) - before; got != 1 {
			t.Errorf("round %d: %d phones allocated, want exactly 1", round, got)
		}
		after, _ := acc.Get(ctx, amy.UserID)
		if *after.AgentID != agentID {
			t.Errorf("round %d: agent id changed", round)
		}
	}
}

// The repair's work list: call-taking identities with no phone, whatever
// their status, and nobody else.
func TestPhonelessCallTakersListsOnlyCallTakersWithoutAPhone(t *testing.T) {
	acc := accountStore(t)
	ctx := context.Background()

	amy, _ := phonelessAccount(t, acc, "amy", "AGENT")
	cara, _ := phonelessAccount(t, acc, "cara", "SUPERVISOR")
	dan, _ := phonelessAccount(t, acc, "dan", "AGENT")
	if _, err := acc.pool.Exec(ctx, `UPDATE users SET status = 'SUSPENDED' WHERE id = $1`, dan.UserID); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	phonelessAccount(t, acc, "boss", "ADMIN")
	if _, err := acc.Provision(ctx, newAccount("wei", "AGENT")); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := acc.Provision(ctx, newAccount("root", "ADMIN")); err != nil {
		t.Fatalf("provision: %v", err)
	}

	ids, err := acc.PhonelessCallTakers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []uuid.UUID{amy.UserID, cara.UserID, dan.UserID}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Errorf("listed %v, want amy, cara and dan %v", ids, want)
	}
}
