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

// DialForm is how a trunk wants a number written on the wire
// (AICC_OUTBOUND_DIAL_FORM). The campaign store and every comparison stay
// canonical E.164; only the string handed to the switch changes.
type DialForm string

const (
	// FormE164 dials "+<cc><number>": the canonical form, unchanged.
	FormE164 DialForm = "E164"
	// FormE164NoPlus dials "<cc><number>".
	FormE164NoPlus DialForm = "E164_NO_PLUS"
	// FormNational dials the number without "+<cc>". A trunk prefix a
	// national number may need (the 0 of a Chinese landline area code, which
	// the canonical form drops) is not restored: that is a known limit.
	FormNational DialForm = "NATIONAL"
)

// ParseDialForm validates a setting; empty means FormE164.
func ParseDialForm(s string) (DialForm, error) {
	switch DialForm(s) {
	case "":
		return FormE164, nil
	case FormE164, FormE164NoPlus, FormNational:
		return DialForm(s), nil
	}
	return "", errors.New("AICC_OUTBOUND_DIAL_FORM must be E164, E164_NO_PLUS or NATIONAL, got " + `"` + s + `"`)
}

// DialString writes a canonical E.164 number in the form a trunk expects.
// countryCode is digits only ("86"). The second result says whether the form
// was applied: it is false, and the number is returned as E.164, when the
// input is not canonical, or for FormNational when countryCode is empty or
// the number does not start with it. An unknown form behaves as FormE164.
func DialString(e164 string, form DialForm, countryCode string) (string, bool) {
	if !isCanonical(e164) {
		return e164, false
	}
	switch form {
	case FormE164NoPlus:
		return e164[1:], true
	case FormNational:
		prefix := "+" + countryCode
		if countryCode == "" || !strings.HasPrefix(e164, prefix) || len(e164) == len(prefix) {
			return e164, false
		}
		return e164[len(prefix):], true
	}
	return e164, true
}

// isCanonical reports whether s is "+", a digit 1-9, then 7 to 14 digits.
func isCanonical(s string) bool {
	if len(s) < 1+1+minTail || len(s) > 1+1+maxTail || s[0] != '+' || s[1] == '0' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
