// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// #289: a stored locale survives a read-modify-write of the account. User
// returns it, `en` for an account that never chose one, and UserUpdate
// replaces the whole account including it, so a client that sends back what
// it read keeps it. Everything runs through the real routes against a real
// database, and the column is read directly rather than trusted from the
// response.

// wireUser is the read shape as a client sees it. Locale is a pointer only so
// an absent or null property is caught rather than read as "".
type wireUser struct {
	UserID          uuid.UUID `json:"userId"`
	Username        string    `json:"username"`
	DisplayName     string    `json:"displayName"`
	Role            string    `json:"role"`
	Status          string    `json:"status"`
	Locale          *string   `json:"locale"`
	ExtensionNumber *string   `json:"extensionNumber"`
}

func strp(s string) *string { return &s }

func showLocale(p *string) string {
	if p == nil {
		return "<null>"
	}
	return fmt.Sprintf("%q", *p)
}

func sameLocale(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// readsAs is what an account with this stored locale reads as.
func readsAs(stored *string) string {
	if stored == nil {
		return "en"
	}
	return *stored
}

// createUser goes through POST /users, so the locale takes the real write path.
func (f *fixture) createUser(admin, username, role string, locale *string) wireUser {
	f.t.Helper()
	body := map[string]any{"username": username, "role": role, "password": seedPassword}
	if locale != nil {
		body["locale"] = *locale
	}
	got := f.do(call{method: http.MethodPost, path: "/users", cookie: admin, body: body})
	if got.status != http.StatusCreated {
		f.t.Fatalf("create %s: status %d (%s): %.200s", username, got.status, got.code, got.body)
	}
	u := decodeUser(f.t, got.body)
	if *u.Locale != readsAs(locale) {
		f.t.Fatalf("create %s answered locale %q, want %q", username, *u.Locale, readsAs(locale))
	}
	return u
}

// decodeUser also holds the contract: locale is a required string, never
// absent and never null.
func decodeUser(t *testing.T, raw []byte) wireUser {
	t.Helper()
	var u wireUser
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatalf("decode user: %v (%.200s)", err, raw)
	}
	if u.Locale == nil {
		t.Fatalf("user has no locale, or a null one: %.200s", raw)
	}
	return u
}

func (f *fixture) listUsers(admin string) map[string]wireUser {
	f.t.Helper()
	got := f.do(call{method: http.MethodGet, path: "/users", cookie: admin})
	if got.status != http.StatusOK {
		f.t.Fatalf("list users: status %d (%s)", got.status, got.code)
	}
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(got.body, &page); err != nil {
		f.t.Fatalf("decode user list: %v", err)
	}
	out := make(map[string]wireUser, len(page.Items))
	for _, raw := range page.Items {
		u := decodeUser(f.t, raw)
		out[u.Username] = u
	}
	return out
}

// storedLocale reads the column, not the response.
func (f *fixture) storedLocale(userID uuid.UUID) *string {
	f.t.Helper()
	var locale *string
	if err := f.st.Pool.QueryRow(context.Background(),
		`SELECT locale FROM users WHERE id = $1`, userID).Scan(&locale); err != nil {
		f.t.Fatalf("read locale: %v", err)
	}
	return locale
}

// putUser sends exactly the read shape's writable fields back.
func (f *fixture) putUser(admin string, u wireUser) reply {
	f.t.Helper()
	return f.do(call{method: http.MethodPut, path: "/users/" + u.UserID.String(), cookie: admin,
		body: map[string]any{
			"username": u.Username, "displayName": u.DisplayName,
			"role": u.Role, "status": u.Status, "locale": u.Locale,
		}})
}

func TestSavingEveryAccountAsReadKeepsItsLocale(t *testing.T) {
	f, _ := phoneFixture(t, "1000-1199")
	admin := f.seedAdmin()

	stored := map[string]*string{}
	named := []struct {
		username, role string
		locale         *string
	}{
		{"li.na", "AGENT", strp("zh")},
		{"amy.o-brien", "AGENT", strp("en")},
		{"zhang_wei.2", "SUPERVISOR", strp("zh")},
		{"ops-admin", "ADMIN", strp("en")},
		{"old.agent", "SUPERVISOR", strp("zh")},
		{"no.pref", "AGENT", nil},
		{"mixed.case", "AGENT", strp("zh-CN")},
	}
	for _, n := range named {
		f.createUser(admin, n.username, n.role, n.locale)
		stored[n.username] = n.locale
	}
	// A page of generated accounts, so the pass covers rows nobody picked.
	pool := []*string{strp("en"), strp("zh"), nil, strp("en"), strp("zh"), nil, strp("zh-Hans1")}
	for i := range 40 {
		username := fmt.Sprintf("gen-%02d", i)
		role := []string{"AGENT", "AGENT", "SUPERVISOR"}[i%3]
		f.createUser(admin, username, role, pool[i%len(pool)])
		stored[username] = pool[i%len(pool)]
	}
	// old.agent: demoted from SUPERVISOR and suspended, with the locale sent
	// back as read.
	old := f.listUsers(admin)["old.agent"]
	old.Role, old.Status = "AGENT", "SUSPENDED"
	if got := f.putUser(admin, old); got.status != http.StatusOK {
		t.Fatalf("demote old.agent: status %d (%s): %.200s", got.status, got.code, got.body)
	}

	// GET /users returns what was stored, and `en` where nothing was.
	listed := f.listUsers(admin)
	for username, locale := range stored {
		if got := *listed[username].Locale; got != readsAs(locale) {
			t.Errorf("GET /users: %s locale %q, want %q", username, got, readsAs(locale))
		}
	}
	if root := listed["root"]; *root.Locale != "en" {
		t.Errorf("root was created without a locale, lists %q, want \"en\"", *root.Locale)
	}

	// Every row, saved exactly as read. An account that had none stored now
	// stores `en`, the value it already read as.
	for username, u := range listed {
		phoneBefore := u.ExtensionNumber
		got := f.putUser(admin, u)
		if got.status != http.StatusOK {
			t.Fatalf("save %s: status %d (%s): %.200s", username, got.status, got.code, got.body)
		}
		answered := decodeUser(t, got.body)
		if *answered.Locale != *u.Locale {
			t.Errorf("save %s answered locale %q, sent %q", username, *answered.Locale, *u.Locale)
		}
		if after := f.storedLocale(u.UserID); !sameLocale(after, u.Locale) {
			t.Errorf("save %s stored locale %s, sent %q", username, showLocale(after), *u.Locale)
		}
		if !sameLocale(answered.ExtensionNumber, phoneBefore) {
			t.Errorf("save %s moved the phone %s → %s", username,
				showLocale(phoneBefore), showLocale(answered.ExtensionNumber))
		}
		if answered.Role != u.Role || answered.Status != u.Status {
			t.Errorf("save %s changed %s/%s → %s/%s", username, u.Role, u.Status, answered.Role, answered.Status)
		}
	}
	if ops := listed["ops-admin"]; ops.ExtensionNumber != nil {
		t.Errorf("an administrator was given phone %s", *ops.ExtensionNumber)
	}

	// The person's own view agrees after all of that.
	var me struct {
		User struct {
			Locale *string `json:"locale"`
		} `json:"user"`
	}
	for _, username := range []string{"li.na", "mixed.case", "no.pref"} {
		cookie := f.signIn(username)
		got := f.do(call{method: http.MethodGet, path: "/auth/me", cookie: cookie})
		if got.status != http.StatusOK {
			t.Fatalf("me as %s: status %d", username, got.status)
		}
		if err := json.Unmarshal(got.body, &me); err != nil {
			t.Fatalf("decode me: %v", err)
		}
		if want := readsAs(stored[username]); !sameLocale(me.User.Locale, &want) {
			t.Errorf("/auth/me as %s: locale %s, want %q", username, showLocale(me.User.Locale), want)
		}
	}
}

// The person's own view reads the same default as the accounts list, before
// any save has written one: /auth/login and /auth/me answer `en` for an
// account with nothing stored, and the stored value otherwise.
func TestSigningInReadsTheSameLocaleAsTheAccountsList(t *testing.T) {
	f, _ := phoneFixture(t, "1000-1199")
	admin := f.seedAdmin()
	for _, tc := range []struct {
		username, role string
		stored         *string
	}{
		{"no.pref", "AGENT", nil},
		{"ops-admin", "ADMIN", nil},
		{"li.na", "AGENT", strp("zh")},
		{"mixed.case", "SUPERVISOR", strp("zh-CN")},
	} {
		created := f.createUser(admin, tc.username, tc.role, tc.stored)
		if stored := f.storedLocale(created.UserID); !sameLocale(stored, tc.stored) {
			t.Fatalf("%s: stored %s before any save, want %s", tc.username, showLocale(stored), showLocale(tc.stored))
		}
		want := readsAs(tc.stored)

		var signedIn struct {
			User struct {
				Locale *string `json:"locale"`
			} `json:"user"`
		}
		body, _ := json.Marshal(map[string]string{"username": tc.username, "password": seedPassword})
		res, err := http.Post(f.server.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("login %s: %v", tc.username, err)
		}
		err = json.NewDecoder(res.Body).Decode(&signedIn)
		res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("login %s: status %d, %v", tc.username, res.StatusCode, err)
		}
		if !sameLocale(signedIn.User.Locale, &want) {
			t.Errorf("/auth/login as %s: locale %s, want %q", tc.username, showLocale(signedIn.User.Locale), want)
		}

		signedIn.User.Locale = nil
		got := f.do(call{method: http.MethodGet, path: "/auth/me", cookie: f.signIn(tc.username)})
		if got.status != http.StatusOK {
			t.Fatalf("me as %s: status %d", tc.username, got.status)
		}
		if err := json.Unmarshal(got.body, &signedIn); err != nil {
			t.Fatalf("decode me: %v", err)
		}
		if !sameLocale(signedIn.User.Locale, &want) {
			t.Errorf("/auth/me as %s: locale %s, want %q", tc.username, showLocale(signedIn.User.Locale), want)
		}
		if listed := f.listUsers(admin)[tc.username]; *listed.Locale != want {
			t.Errorf("GET /users: %s locale %q, want %q", tc.username, *listed.Locale, want)
		}
	}
}

func TestAnEditKeepsTheLocaleAndAnExplicitValueReplacesIt(t *testing.T) {
	f, _ := phoneFixture(t, "1000-1199")
	admin := f.seedAdmin()
	lina := f.createUser(admin, "li.na", "AGENT", strp("zh"))
	amy := f.createUser(admin, "amy.o-brien", "AGENT", strp("en"))

	steps := []struct {
		name   string
		edit   func(*wireUser)
		target *wireUser
		want   string
	}{
		{"rename", func(u *wireUser) { u.DisplayName = "Li Na (night shift)" }, &lina, "zh"},
		{"suspend", func(u *wireUser) { u.Status = "SUSPENDED" }, &amy, "en"},
		{"change", func(u *wireUser) { u.Locale = strp("en") }, &lina, "en"},
		{"set again", func(u *wireUser) { u.Locale = strp("zh-Hans1") }, &lina, "zh-Hans1"},
	}
	for _, step := range steps {
		u := *step.target
		step.edit(&u)
		got := f.putUser(admin, u)
		if got.status != http.StatusOK {
			t.Fatalf("%s: status %d (%s): %.200s", step.name, got.status, got.code, got.body)
		}
		*step.target = decodeUser(t, got.body)
		if *step.target.Locale != step.want {
			t.Errorf("%s: answered locale %q, want %q", step.name, *step.target.Locale, step.want)
		}
		if stored := f.storedLocale(u.UserID); !sameLocale(stored, &step.want) {
			t.Errorf("%s: stored locale %s, want %q", step.name, showLocale(stored), step.want)
		}
	}

	// A whole replacement: leaving the property out stores no preference,
	// which reads as `en`.
	got := f.do(call{method: http.MethodPut, path: "/users/" + lina.UserID.String(), cookie: admin,
		body: map[string]any{"username": lina.Username, "displayName": lina.DisplayName,
			"role": lina.Role, "status": lina.Status}})
	if got.status != http.StatusOK {
		t.Fatalf("save without locale: status %d (%s)", got.status, got.code)
	}
	if stored := f.storedLocale(lina.UserID); stored != nil {
		t.Errorf("save without locale stored %s, want null", showLocale(stored))
	}
	if answered := decodeUser(t, got.body); *answered.Locale != "en" {
		t.Errorf("save without locale answered %q, want \"en\"", *answered.Locale)
	}
	if listed := f.listUsers(admin)["li.na"]; *listed.Locale != "en" {
		t.Errorf("GET /users after a save without locale: %q, want \"en\"", *listed.Locale)
	}
}

// A refused update leaves the locale where it was.
func TestARefusedUpdateLeavesTheLocaleAlone(t *testing.T) {
	f, _ := phoneFixture(t, "1000-1199")
	admin := f.seedAdmin()
	root := f.listUsers(admin)["root"]
	if _, err := f.st.Pool.Exec(context.Background(),
		`UPDATE users SET locale = 'zh' WHERE id = $1`, root.UserID); err != nil {
		t.Fatalf("set root's locale: %v", err)
	}

	refusals := []struct {
		name   string
		edit   func(*wireUser)
		status int
	}{
		{"last admin demoted", func(u *wireUser) { u.Role = "AGENT" }, http.StatusConflict},
		{"last admin suspended", func(u *wireUser) { u.Status = "SUSPENDED" }, http.StatusConflict},
		{"bad username", func(u *wireUser) { u.Username = "root admin" }, http.StatusUnprocessableEntity},
	}
	for _, r := range refusals {
		u := root
		u.Locale = strp("en")
		r.edit(&u)
		got := f.putUser(admin, u)
		if got.status != r.status {
			t.Fatalf("%s: status %d (%s), want %d", r.name, got.status, got.code, r.status)
		}
		if stored := f.storedLocale(root.UserID); !sameLocale(stored, strp("zh")) {
			t.Errorf("%s: locale %s after the refusal, want \"zh\"", r.name, showLocale(stored))
		}
	}
}

// #268's "Assign a phone" saves the account as read; the phone is bound and
// the locale stays.
func TestAssigningAPhoneKeepsTheLocale(t *testing.T) {
	f, _ := phoneFixture(t, "1000-1099")
	admin := f.seedAdmin()
	for _, tc := range []struct {
		username string
		stored   *string
	}{
		{"zhang_wei.2", strp("zh")},
		// Nothing stored reads `en`, and the save stores what it read.
		{"no.pref", nil},
	} {
		before := f.phoneless(tc.username, "SUPERVISOR")
		if _, err := f.st.Pool.Exec(context.Background(),
			`UPDATE users SET locale = $2 WHERE id = $1`, before.UserID, tc.stored); err != nil {
			t.Fatalf("set locale: %v", err)
		}
		read := f.listUsers(admin)[tc.username]
		if *read.Locale != readsAs(tc.stored) || read.ExtensionNumber != nil {
			t.Fatalf("%s: read locale %q phone %s, want %q and none",
				tc.username, *read.Locale, showLocale(read.ExtensionNumber), readsAs(tc.stored))
		}

		got := f.putUser(admin, read)
		if got.status != http.StatusOK {
			t.Fatalf("%s: status %d (%s): %.200s", tc.username, got.status, got.code, got.body)
		}
		if after := decodeUser(t, got.body); after.ExtensionNumber == nil {
			t.Errorf("%s: no phone bound: %.200s", tc.username, got.body)
		}
		want := readsAs(tc.stored)
		if stored := f.storedLocale(read.UserID); !sameLocale(stored, &want) {
			t.Errorf("%s: locale %s after assigning a phone, want %q", tc.username, showLocale(stored), want)
		}
	}
}
