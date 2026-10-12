// SPDX-License-Identifier: Apache-2.0

// Package phonenum holds the one definition of a canonical telephone number.
//
// Every number the dialer, the do-not-call list and the contact importer
// compare is canonical E.164: a "+" and 8 to 15 digits, the first not zero
// (design draft for campaigns, section 9.1). Anything typed by a person goes
// through Canonical first, so "+8613800138000" and "138 0013 8000" are one
// number once the default country code is 86.
//
// The dial path (internal/outbound) does not canonicalise: a single call may
// still carry a national number and is dialled as sent, with at most one
// leading "+", and the CDR stores the number as dialled. Canonical is for the
// places that compare numbers.
package phonenum

import (
	"errors"
	"strings"
)

// Reason says why a number was refused. The values are the per-row error
// codes of an import and are byte-identical in JSON.
type Reason string

const (
	// NumberEmpty means nothing was left once separators were removed.
	NumberEmpty Reason = "NUMBER_EMPTY"
	// NumberInvalid means what was left is not a canonical E.164 number.
	NumberInvalid Reason = "NUMBER_INVALID"
)

// Error is the typed refusal Canonical returns.
type Error struct {
	Reason Reason
}

func (e *Error) Error() string { return "phonenum: " + strings.ToLower(string(e.Reason)) }

// ReasonOf returns the reason carried by err, or "" when err is not an *Error.
func ReasonOf(err error) Reason {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Reason
	}
	return ""
}

const (
	minTail = 7  // digits after the leading non-zero digit
	maxTail = 14 // so 8 to 15 digits in all
)

// Canonical turns what a person typed into canonical E.164.
//
//  1. spaces, dashes, dots and round brackets are removed;
//  2. a leading "+" is kept, and a leading "00" becomes "+";
//  3. otherwise one leading trunk "0" is dropped and "+" plus
//     defaultCountryCode is prefixed;
//  4. the result must be "+", a digit 1-9, then 7 to 14 more digits.
//
// defaultCountryCode is digits only, without "+" (for example "86"). With it
// empty a national number cannot be completed and is refused.
func Canonical(raw, defaultCountryCode string) (string, error) {
	var b strings.Builder
	for _, r := range raw {
		switch r {
		case ' ', '-', '.', '(', ')':
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	if s == "" {
		return "", &Error{NumberEmpty}
	}

	switch {
	case strings.HasPrefix(s, "+"):
	case strings.HasPrefix(s, "00"):
		s = "+" + s[2:]
	default:
		if defaultCountryCode == "" {
			return "", &Error{NumberInvalid}
		}
		s = "+" + defaultCountryCode + strings.TrimPrefix(s, "0")
	}
	if !IsCanonical(s) {
		return "", &Error{NumberInvalid}
	}
	return s, nil
}

// IsCanonical reports whether s is already canonical E.164.
func IsCanonical(s string) bool {
	if len(s) < 2+minTail || len(s) > 2+maxTail || s[0] != '+' {
		return false
	}
	if s[1] < '1' || s[1] > '9' {
		return false
	}
	for i := 2; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
