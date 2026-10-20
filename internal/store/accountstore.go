// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasonyang/ai-native-callcenter/internal/catalog"
	"github.com/rasonyang/ai-native-callcenter/internal/store/queries"
)

// AccountStore provisions and edits accounts.
//
// An account is up to three rows: the user, the ACD identity of somebody who
// takes calls, and the phone they take them at. They are written together
// because a call-taking account is not usable without all three, and a
// half-provisioned one is not a state anybody could act on — nothing in the
// product can tell an agent who never got a phone from one whose phone was
// deliberately unbound.
type AccountStore struct {
	q    *queries.Queries
	pool *pgxpool.Pool
}

// Accounts returns the account view of the store.
func (s *Store) Accounts() *AccountStore { return &AccountStore{q: s.Queries, pool: s.Pool} }

// Errors an account write can produce.
var (
	// ErrUsernameTaken is the unique constraint, named.
	ErrUsernameTaken = errors.New("store: username already taken")
	// ErrNoSuchAccount is asked to change an account that is not there.
	ErrNoSuchAccount = errors.New("store: no such account")
	// ErrLastAdmin refuses to remove the last way into the product.
	ErrLastAdmin = errors.New("store: the last active administrator cannot be demoted or suspended")
)

// Account is an account and whatever belongs to it.
type Account struct {
	UserID      uuid.UUID
	Username    string
	DisplayName string
	Role        string
	Status      string
	Locale      *string
	// AgentID and the two fields below are absent for an account that does not
	// take calls. An administrator is a user but not an agent.
	AgentID         *uuid.UUID
	CallcenterName  string
	ExtensionNumber string
}

// NewAccount is an account to create.
type NewAccount struct {
	Username     string
	PasswordHash string
	DisplayName  string
	Role         string
	Locale       *string
	// CallcenterName and SIPPassword are only read when the role takes calls.
	CallcenterName string
	SIPPassword    string
	ExtensionLow   int
	ExtensionHigh  int
}

// Provision creates an account and, for a role that takes calls, its ACD
// identity and a phone from the extension pool — all in one transaction.
//
// The user is inserted before the pool is locked. A username somebody already
// has must be refused by the unique constraint, not discovered while holding a
// lock every other allocation is queueing behind; and because the whole thing
// is one transaction, a refused username gives its number back rather than
// burning one out of the pool on every mistyped form.
func (a *AccountStore) Provision(ctx context.Context, in NewAccount) (Account, error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("provision account: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := a.q.WithTx(tx)

	user, err := qtx.CreateUser(ctx, queries.CreateUserParams{
		ID:           uuid.Must(uuid.NewV7()),
		Username:     in.Username,
		PasswordHash: in.PasswordHash,
		DisplayName:  in.DisplayName,
		Role:         in.Role,
		Status:       "ACTIVE",
		Locale:       in.Locale,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return Account{}, ErrUsernameTaken
		}
		return Account{}, fmt.Errorf("create user %s: %w", in.Username, err)
	}

	account := Account{
		UserID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
		Role: user.Role, Status: user.Status, Locale: user.Locale,
	}

	if takesCalls(in.Role) {
		agent, phone, err := provisionAgentTx(ctx, qtx, user.ID, in)
		if err != nil {
			return Account{}, err
		}
		account.AgentID = &agent.ID
		account.CallcenterName = agent.CallcenterName
		account.ExtensionNumber = phone.Number
	}

	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("commit account %s: %w", in.Username, err)
	}
	return account, nil
}

// EnsureAgentIdentity gives a call-taking account the identity and phone its
// role needs, and does nothing when it already has them.
//
// Two accounts reach it. An administrator promoted to supervisor has never had
// either, and gets both. An agent whose identity exists without a phone — an
// account from before a phone came with every account — gets a phone, bound
// to the identity it already has: that identity's id is what its state
// history and filed wrap-ups hang off, so a new one would orphan the record of
// what the person did.
//
// It decides from the account as it stands inside the transaction, not as the
// caller last read it. The account's row is locked first, so two edits of one
// phoneless account at once queue here and the second finds the phone the
// first gave; the identity's row is locked next, so a binding made meanwhile
// through the agent configuration is waited for rather than overwritten. An
// account whose role takes no calls is left as it is, whatever identity it
// kept from before a demotion. The reverse has no counterpart on purpose — see
// Update.
func (a *AccountStore) EnsureAgentIdentity(ctx context.Context, userID uuid.UUID,
	in NewAccount) (Account, error) {
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return Account{}, fmt.Errorf("provision agent identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := a.q.WithTx(tx)

	role, err := qtx.LockUserForProvisioning(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrNoSuchAccount
		}
		return Account{}, fmt.Errorf("lock account: %w", err)
	}
	// The answers that change nothing are read on the transaction's own
	// connection. Asking the pool for a second one while holding these locks
	// is how a burst of edits on one account would starve it: every waiter
	// holds a connection, and the holder could not get one to finish with.
	if !takesCalls(role) {
		return readAccount(ctx, qtx, userID)
	}

	agent, err := qtx.GetAgentByUserIDForUpdate(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, _, err := provisionAgentTx(ctx, qtx, userID, in); err != nil {
			return Account{}, err
		}
	case err != nil:
		return Account{}, fmt.Errorf("look up agent identity: %w", err)
	case agent.DefaultExtensionID != nil:
		return readAccount(ctx, qtx, userID) // already has one; nothing to do
	default:
		if err := givePhoneTx(ctx, qtx, agent.ID, in); err != nil {
			return Account{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Account{}, fmt.Errorf("commit agent identity: %w", err)
	}
	return a.Get(ctx, userID)
}

// PhonelessCallTakers is every call-taking account whose identity has no
// phone, whatever its status: the work list of the startup repair. A
// suspended account is on it, because reinstating somebody should not hand
// back an account that cannot sign in.
func (a *AccountStore) PhonelessCallTakers(ctx context.Context) ([]uuid.UUID, error) {
	ids, err := a.q.ListPhonelessCallTakers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list phoneless agents: %w", err)
	}
	return ids, nil
}

// givePhoneTx allocates a phone from the pool and binds it to an identity that
// has none, on a transaction somebody else owns.
func givePhoneTx(ctx context.Context, qtx *queries.Queries, agentID uuid.UUID, in NewAccount) error {
	phone, err := allocateExtensionTx(ctx, qtx, phoneFor(in), in.ExtensionLow, in.ExtensionHigh)
	if err != nil {
		return err
	}
	n, err := qtx.GiveAgentPhone(ctx, queries.GiveAgentPhoneParams{
		ID: agentID, DefaultExtensionID: &phone.ID,
	})
	if err != nil {
		return fmt.Errorf("bind phone %s: %w", phone.Number, err)
	}
	if n != 1 {
		// Unreachable while the caller holds the identity's row lock; said
		// rather than assumed, because committing would leave a phone nobody
		// is bound to.
		return fmt.Errorf("bind phone %s: the identity already has one", phone.Number)
	}
	return nil
}

// phoneFor is the extension a person's phone is created as.
func phoneFor(in NewAccount) catalog.Extension {
	return catalog.Extension{
		ID:       uuid.Must(uuid.NewV7()),
		Password: in.SIPPassword,
		// Named after the person rather than the number: a directory of
		// "Extension 1042" tells whoever answers the switch's questions
		// nothing about who sits there.
		DisplayName: in.DisplayName,
		IsEnabled:   true,
	}
}

// provisionAgentTx writes the ACD identity and its phone on a transaction
// somebody else owns.
func provisionAgentTx(ctx context.Context, qtx *queries.Queries, userID uuid.UUID,
	in NewAccount) (queries.Agent, queries.Extension, error) {
	phone, err := allocateExtensionTx(ctx, qtx, phoneFor(in), in.ExtensionLow, in.ExtensionHigh)
	if err != nil {
		return queries.Agent{}, queries.Extension{}, err
	}

	agent, err := qtx.CreateAgent(ctx, queries.CreateAgentParams{
		ID:             uuid.Must(uuid.NewV7()),
		UserID:         userID,
		CallcenterName: in.CallcenterName,
		// Off for everyone now: the setting left the account form (W11.3) and
		// the column stays for the deployments that had it on.
		IsAutoAnswer:       false,
		DefaultExtensionID: &phone.ID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return queries.Agent{}, queries.Extension{}, ErrUsernameTaken
		}
		return queries.Agent{}, queries.Extension{}, fmt.Errorf("create agent identity: %w", err)
	}
	return agent, phone, nil
}

// Changes is what an edit sets. Every field is written, so an edit is a whole
// replacement rather than a patch nobody can reason about.
type Changes struct {
	Username    string
	DisplayName string
	Role        string
	Status      string
	Locale      *string
}

// Update rewrites an account.
//
// It never removes an ACD identity. An account demoted out of a call-taking
// role keeps its agent row, because that row is what its state history and its
// filed wrap-ups hang off — the foreign keys cascade, so deleting it is
// destroying the record of what the person did, which is not what "change this
// person's role" asks for.
func (a *AccountStore) Update(ctx context.Context, userID uuid.UUID, in Changes) (Account, error) {
	if in.Role != "ADMIN" || in.Status != "ACTIVE" {
		others, err := a.q.CountOtherActiveAdmins(ctx, userID)
		if err != nil {
			return Account{}, fmt.Errorf("count administrators: %w", err)
		}
		current, err := a.q.GetUserByID(ctx, userID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Account{}, ErrNoSuchAccount
			}
			return Account{}, fmt.Errorf("read account: %w", err)
		}
		if current.Role == "ADMIN" && current.Status == "ACTIVE" && others == 0 {
			return Account{}, ErrLastAdmin
		}
	}

	if _, err := a.q.UpdateUser(ctx, queries.UpdateUserParams{
		ID: userID, Username: in.Username, DisplayName: in.DisplayName,
		Role: in.Role, Status: in.Status, Locale: in.Locale,
	}); err != nil {
		if isUniqueViolation(err) {
			return Account{}, ErrUsernameTaken
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrNoSuchAccount
		}
		return Account{}, fmt.Errorf("update account %s: %w", userID, err)
	}
	return a.Get(ctx, userID)
}

// List is every account, by username.
func (a *AccountStore) List(ctx context.Context) ([]Account, error) {
	rows, err := a.q.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	out := make([]Account, 0, len(rows))
	for _, r := range rows {
		out = append(out, Account{
			UserID: r.ID, Username: r.Username, DisplayName: r.DisplayName,
			Role: r.Role, Status: r.Status, Locale: r.Locale,
			AgentID:         r.AgentID,
			CallcenterName:  deref(r.CallcenterName),
			ExtensionNumber: deref(r.ExtensionNumber),
		})
	}
	return out, nil
}

// Get is one account.
func (a *AccountStore) Get(ctx context.Context, userID uuid.UUID) (Account, error) {
	return readAccount(ctx, a.q, userID)
}

// readAccount is Get on whichever connection the caller is using.
func readAccount(ctx context.Context, q *queries.Queries, userID uuid.UUID) (Account, error) {
	r, err := q.GetAccount(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrNoSuchAccount
		}
		return Account{}, fmt.Errorf("read account %s: %w", userID, err)
	}
	return Account{
		UserID: r.ID, Username: r.Username, DisplayName: r.DisplayName,
		Role: r.Role, Status: r.Status, Locale: r.Locale,
		AgentID:         r.AgentID,
		CallcenterName:  deref(r.CallcenterName),
		ExtensionNumber: deref(r.ExtensionNumber),
	}, nil
}

// takesCalls reports whether a role gets an ACD identity and a phone.
//
// An administrator is a user but not an agent: they configure the platform and
// never appear in a queue. Both other roles do take calls — a supervisor who
// cannot pick one up is not supervising a call centre.
func takesCalls(role string) bool { return role == "AGENT" || role == "SUPERVISOR" }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
