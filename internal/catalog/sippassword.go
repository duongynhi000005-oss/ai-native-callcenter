// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"crypto/rand"
	"encoding/base32"
)

// NewSIPPassword mints a phone credential nobody has to invent.
//
// It is never returned by any write: an operator reads it back through the
// extension's own reveal endpoint, which is ADMIN-only and audited (D5). Long
// enough that it is not worth guessing, and unpadded base32 so it survives
// being typed into a phone's configuration by hand. Shared by every path that
// gives a person a phone — creating an account, editing one that lacks a
// phone, and the startup repair — so no two of them mint a different kind.
func NewSIPPassword() (string, error) {
	var raw [15]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]), nil
}
