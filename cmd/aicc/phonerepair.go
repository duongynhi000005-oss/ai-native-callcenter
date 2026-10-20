// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// phoneRepairer is the account surface the startup repair writes through: the
// same store path an edit of the account takes (#268).
type phoneRepairer interface {
	PhonelessCallTakers(ctx context.Context) ([]uuid.UUID, error)
	Get(ctx context.Context, userID uuid.UUID) (store.Account, error)
	EnsureAgentIdentity(ctx context.Context, userID uuid.UUID, in store.NewAccount) (store.Account, error)
}

// repairPhonelessAgents gives every call-taking agent identity without a
// phone one from the extension pool, and returns the accounts it repaired.
//
// Accounts from before a phone came with every account have an identity and
// no phone, and such an agent cannot sign in: sign-in happens only at the
// bound extension. A migration cannot do this, because the pool is
// AICC_EXTENSION_RANGE and a migration does not read the configuration; so it
// runs here, on every start, and finds nothing to do once the rows are right.
// A suspended account is repaired too, so that reinstating somebody does not
// hand back an account that cannot sign in. An administrator's leftover
// identity is not on the list.
//
// Nothing here stops the server. A pool with no numbers left is logged and
// ends the pass — every further allocation would fail the same way — and the
// operator's answer is a wider range and a restart; any other failure is
// logged and the next account is tried.
func repairPhonelessAgents(ctx context.Context, accounts phoneRepairer, low, high int,
	log *slog.Logger) []store.Account {
	ids, err := accounts.PhonelessCallTakers(ctx)
	if err != nil {
		log.ErrorContext(ctx, "could not look for agents without a phone", "error", err)
		return nil
	}
	repaired := make([]store.Account, 0, len(ids))
	for i, id := range ids {
		account, err := accounts.Get(ctx, id)
		if err != nil {
			log.ErrorContext(ctx, "could not read an agent without a phone", "userId", id, "error", err)
			continue
		}
		sipPassword, err := catalog.NewSIPPassword()
		if err != nil {
			log.ErrorContext(ctx, "could not mint a phone credential", "error", err)
			return repaired
		}
		got, err := accounts.EnsureAgentIdentity(ctx, id, store.NewAccount{
			DisplayName:   account.DisplayName,
			SIPPassword:   sipPassword,
			ExtensionLow:  low,
			ExtensionHigh: high,
		})
		switch {
		case errors.Is(err, catalog.ErrPoolExhausted):
			log.ErrorContext(ctx, "the extension range has no free number for an agent without a phone; "+
				"widen AICC_EXTENSION_RANGE and restart",
				"username", account.Username, "left", len(ids)-i, "low", low, "high", high)
			return repaired
		case err != nil:
			log.ErrorContext(ctx, "could not give an agent a phone",
				"username", account.Username, "error", err)
			continue
		case got.ExtensionNumber == "":
			// Changed under us (demoted to ADMIN since the list was read);
			// the store left it alone and so do we.
			continue
		}
		log.InfoContext(ctx, "gave a phone to an agent without one",
			"username", got.Username, "agentId", got.AgentID, "extension", got.ExtensionNumber,
			"status", got.Status)
		repaired = append(repaired, got)
	}
	if len(repaired) > 0 {
		log.InfoContext(ctx, "agents without a phone repaired", "agents", len(repaired))
	}
	return repaired
}

// mirrorOnFirstConnect tells the switch about the repaired agents once it is
// reachable, as creating an account does for a new one.
//
// The repair runs before the link to the switch is up, so mirroring then would
// be dropped. The reconnect pass mirrors only agents who are signed in, and a
// repaired agent cannot be — until now they had no phone to sign in at. Once
// is enough: from then on they are like any other signed-out agent, mirrored
// again when they sign in.
func mirrorOnFirstConnect(repaired []store.Account,
	mirror func(ctx context.Context, agentID uuid.UUID)) func(context.Context) {
	var isDone atomic.Bool
	return func(ctx context.Context) {
		if len(repaired) == 0 || isDone.Swap(true) {
			return
		}
		for _, a := range repaired {
			if a.AgentID != nil {
				mirror(ctx, *a.AgentID)
			}
		}
	}
}
