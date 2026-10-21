// SPDX-License-Identifier: Apache-2.0

// Package egress decides where the platform may open a connection on a flow
// author's behalf. A flow's HTTP tool names its own backend, and anyone who can
// edit a flow can therefore point the server at an address: without a policy,
// that is a way to make the server fetch its own cloud metadata, or reach a
// public host nobody operating the deployment agreed to send call data to.
//
// The policy is the deployment's (issue #286): the LAN is open, the public
// internet needs an allowlist (AICC_TOOL_ALLOWED_HOSTS), and link-local and
// unspecified addresses are never reached. It is decided on the address the
// connection is about to be made to, after resolution, so a name that changes
// what it resolves to between two calls cannot slip through.
package egress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Rule names why an address was allowed or refused. It is what a refusal's log
// line carries, so an operator can tell a missing allowlist entry from an
// address no entry could ever open.
type Rule string

const (
	// RulePrivate allows a private (RFC 1918, fc00::/7) destination.
	RulePrivate Rule = "PRIVATE"
	// RuleLoopback allows 127.0.0.0/8 and ::1.
	RuleLoopback Rule = "LOOPBACK"
	// RuleAllowedHost allows a public destination whose host is listed.
	RuleAllowedHost Rule = "ALLOWED_HOST"
	// RuleLinkLocal refuses 169.254.0.0/16 and fe80::/10, the cloud metadata
	// address among them, whether listed or not.
	RuleLinkLocal Rule = "LINK_LOCAL"
	// RuleCloudMetadata refuses fd00:ec2::/32, where AWS serves its instance
	// metadata (fd00:ec2::254) and EKS Pod Identity (fd00:ec2::23) over IPv6.
	// It sits inside the private fc00::/7, so it is checked first, and it is
	// refused whether listed or not.
	RuleCloudMetadata Rule = "CLOUD_METADATA"
	// RuleUnspecified refuses 0.0.0.0/8 and ::, which reach the local host on
	// some systems while looking like nothing at all.
	RuleUnspecified Rule = "UNSPECIFIED"
	// RuleMulticast refuses multicast and broadcast destinations, which no
	// request-and-answer backend lives at.
	RuleMulticast Rule = "MULTICAST"
	// RulePublicNotListed refuses a public destination whose host is not in
	// AICC_TOOL_ALLOWED_HOSTS.
	RulePublicNotListed Rule = "PUBLIC_NOT_LISTED"
	// RuleNonCanonical refuses a host written as an IP address in a form other
	// than the standard one (2130706433, 0x7f.1, 017.0.0.1). Resolvers disagree
	// on what such a name means, so it is refused rather than guessed at.
	RuleNonCanonical Rule = "NON_CANONICAL_ADDRESS"
)

// Class is what kind of destination an address is.
type Class int

const (
	ClassPublic Class = iota
	ClassPrivate
	ClassLoopback
	ClassLinkLocal
	ClassUnspecified
	ClassMulticast
	ClassCloudMetadata
)

var (
	privatePrefixes = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("fc00::/7"),
	}
	loopbackPrefixes = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("::1/128"),
	}
	linkLocalPrefixes = []netip.Prefix{
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("fe80::/10"),
	}
	unspecifiedPrefixes = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("::/128"),
	}
	multicastPrefixes = []netip.Prefix{
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("255.255.255.255/32"),
		netip.MustParsePrefix("ff00::/8"),
	}
	cloudMetadataPrefixes = []netip.Prefix{
		netip.MustParsePrefix("fd00:ec2::/32"),
	}
	// nat64Prefix is the well-known NAT64 prefix (RFC 6052). Behind a NAT64
	// gateway 64:ff9b::a9fe:a9fe is 169.254.169.254, so it is judged by the
	// IPv4 address it carries.
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	// nat64LocalPrefix is the local-use NAT64 prefix (RFC 8215). It is a /48,
	// so RFC 6052 places the IPv4 address in bits 48-63 and 72-103, around the
	// reserved octet at bits 64-71.
	nat64LocalPrefix = netip.MustParsePrefix("64:ff9b:1::/48")
)

// Classify says what kind of destination addr is. An IPv4-mapped IPv6 address
// (::ffff:a.b.c.d) and a NAT64 one (64:ff9b::/96, 64:ff9b:1::/48) are judged
// by the IPv4 address inside, so ::ffff:169.254.169.254 is link-local and
// ::ffff:10.0.0.1 private.
func Classify(addr netip.Addr) Class {
	addr = embeddedIPv4(addr.WithZone("").Unmap())
	switch {
	case containedIn(cloudMetadataPrefixes, addr):
		return ClassCloudMetadata
	case containedIn(unspecifiedPrefixes, addr):
		return ClassUnspecified
	case containedIn(linkLocalPrefixes, addr):
		return ClassLinkLocal
	case containedIn(multicastPrefixes, addr):
		return ClassMulticast
	case containedIn(loopbackPrefixes, addr):
		return ClassLoopback
	case containedIn(privatePrefixes, addr):
		return ClassPrivate
	default:
		return ClassPublic
	}
}

// embeddedIPv4 returns the IPv4 address a NAT64 address carries, or addr.
func embeddedIPv4(addr netip.Addr) netip.Addr {
	if !addr.Is6() {
		return addr
	}
	raw := addr.As16()
	switch {
	case nat64Prefix.Contains(addr):
		return netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]})
	case nat64LocalPrefix.Contains(addr):
		return netip.AddrFrom4([4]byte{raw[6], raw[7], raw[9], raw[10]})
	}
	return addr
}

func containedIn(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// Entry is one AICC_TOOL_ALLOWED_HOSTS item: a host name, an IP address, or
// "*.domain" for every subdomain of domain, each with an optional port.
type Entry struct {
	// Host is lower case, without a trailing dot. For a wildcard it is the
	// domain after "*.".
	Host string
	// Port is empty when the entry allows every port.
	Port string
	// IsWildcard says Host is matched as a suffix: "*.example.com" allows
	// api.example.com but not example.com itself.
	IsWildcard bool
	// addr is set when Host is an IP address.
	addr netip.Addr
}

func (e Entry) String() string {
	host := e.Host
	if e.IsWildcard {
		host = "*." + host
	}
	if e.Port == "" {
		return host
	}
	return net.JoinHostPort(host, e.Port)
}

// hostLabel is one DNS label: letters, digits and hyphens, not at either end.
// Underscores are allowed too, since internal names carry them.
var hostLabel = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?$`)

// ParseAllowList reads AICC_TOOL_ALLOWED_HOSTS: entries separated by commas,
// each "host", "host:port", "*.domain" or "*.domain:port". An IPv6 address
// with a port is written in brackets ("[2001:db8::1]:443"). Empty items are
// skipped; anything else that is not one of those shapes is an error.
func ParseAllowList(raw string) ([]Entry, error) {
	var entries []Entry
	for item := range strings.SplitSeq(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		entry, err := parseEntry(item)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func parseEntry(item string) (Entry, error) {
	host, port := item, ""
	if addr, err := netip.ParseAddr(strings.Trim(item, "[]")); err == nil {
		return addressEntry(item, addr, "")
	}
	if h, p, err := net.SplitHostPort(item); err == nil {
		host, port = h, p
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return Entry{}, fmt.Errorf("%q: the port must be a number from 1 to 65535", item)
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			return addressEntry(item, addr, port)
		}
	} else if strings.Contains(item, ":") {
		return Entry{}, fmt.Errorf("%q is not a host or host:port (an IPv6 address with a port goes in brackets)", item)
	}

	entry := Entry{Port: port}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		entry.IsWildcard = true
		host = rest
		// "*.com" would open a whole top-level domain.
		if !strings.Contains(host, ".") {
			return Entry{}, fmt.Errorf("%q: a wildcard needs a domain of at least two labels after \"*.\"", item)
		}
	}
	if host == "" || !isHostName(host) {
		return Entry{}, fmt.Errorf("%q is not a host name, an IP address or \"*.\" followed by a domain", item)
	}
	if !entry.IsWildcard && isNonCanonicalNumeric(host) {
		return Entry{}, fmt.Errorf("%q: write an IP address in its standard form", item)
	}
	entry.Host = host
	return entry, nil
}

func addressEntry(item string, addr netip.Addr, port string) (Entry, error) {
	if addr.Zone() != "" {
		return Entry{}, fmt.Errorf("%q: an address with a zone cannot be listed", item)
	}
	addr = addr.Unmap()
	return Entry{Host: addr.String(), Port: port, addr: addr}, nil
}

func isHostName(host string) bool {
	if len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) > 63 || !hostLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// numericPart is one part of an IPv4 address as inet_aton reads it: decimal,
// octal (leading 0) or hexadecimal (0x).
var numericPart = regexp.MustCompile(`^(0[xX][0-9a-fA-F]*|[0-9]+)$`)

// isNonCanonicalNumeric reports a host that some resolver would read as an
// IPv4 address but that is not one in the standard dotted-quad form:
// 2130706433, 0x7f.1, 017.0.0.1. A standard address is not one of these.
func isNonCanonicalNumeric(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(host, "."), ".")
	if len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		if !numericPart.MatchString(part) {
			return false
		}
	}
	return true
}

// Policy is the deployment's outbound host policy.
type Policy struct {
	allowed []Entry
}

// NewPolicy builds the policy from the allowlist. alwaysAllowed are hosts that
// are allowed on every port whatever the list says: the host of
// AICC_BOT_BACKEND_BASE, which the operator configured as the deployment's own
// backend. An empty or unparsable one is skipped.
func NewPolicy(allowed []Entry, alwaysAllowed ...string) *Policy {
	p := &Policy{allowed: append([]Entry(nil), allowed...)}
	for _, host := range alwaysAllowed {
		if host == "" {
			continue
		}
		if entry, err := parseEntry(host); err == nil && !entry.IsWildcard {
			entry.Port = ""
			p.allowed = append(p.allowed, entry)
		}
	}
	return p
}

// Refusal is a connection the policy did not allow. It names the host the tool
// asked for, the address that host resolved to and the rule that refused it,
// and nothing else: no path, no query, no header.
type Refusal struct {
	Host    string
	Address string
	Rule    Rule
}

func (r *Refusal) Error() string {
	var why string
	switch r.Rule {
	case RuleLinkLocal:
		why = "a link-local address, which is never reached"
	case RuleCloudMetadata:
		why = "a cloud metadata address, which is never reached"
	case RuleUnspecified:
		why = "an unspecified address, which is never reached"
	case RuleMulticast:
		why = "a multicast or broadcast address"
	case RuleNonCanonical:
		why = "an IP address written in a non-standard form"
	case RulePublicNotListed:
		why = "a public address, and the host is not in AICC_TOOL_ALLOWED_HOSTS"
	default:
		why = string(r.Rule)
	}
	if r.Address == "" {
		return fmt.Sprintf("connection to %s refused by the outbound host policy: %s", r.Host, why)
	}
	return fmt.Sprintf("connection to %s (%s) refused by the outbound host policy: %s", r.Host, r.Address, why)
}

// IsRefusal reports whether err is, or wraps, a policy refusal, and returns it.
func IsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// Check decides one connection: host is what the tool asked for (a name or an
// IP literal), port its port, and addr the address host resolved to and the
// connection would be made to. It returns the rule that allowed it, or a
// *Refusal.
func (p *Policy) Check(host, port string, addr netip.Addr) (Rule, error) {
	refuse := func(rule Rule) (Rule, error) {
		return rule, &Refusal{Host: host, Address: netip.AddrPortFrom(addr, portNumber(port)).String(), Rule: rule}
	}
	switch Classify(addr) {
	case ClassUnspecified:
		return refuse(RuleUnspecified)
	case ClassCloudMetadata:
		return refuse(RuleCloudMetadata)
	case ClassLinkLocal:
		return refuse(RuleLinkLocal)
	case ClassMulticast:
		return refuse(RuleMulticast)
	case ClassLoopback:
		return RuleLoopback, nil
	case ClassPrivate:
		return RulePrivate, nil
	}
	if p.isListed(host, port) {
		return RuleAllowedHost, nil
	}
	return refuse(RulePublicNotListed)
}

// isListed reports whether the host the tool named, on this port, matches an
// allowlist entry.
func (p *Policy) isListed(host, port string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	hostAddr, parseErr := netip.ParseAddr(host)
	hostIsAddr := parseErr == nil
	if hostIsAddr {
		hostAddr = hostAddr.Unmap()
	}
	for _, e := range p.allowed {
		if e.Port != "" && e.Port != port {
			continue
		}
		switch {
		case e.addr.IsValid():
			if hostIsAddr && e.addr == hostAddr {
				return true
			}
		case hostIsAddr:
			// A name entry never matches an address: listing a name is not
			// listing whatever it happens to resolve to today.
		case e.IsWildcard:
			if strings.HasSuffix(host, "."+e.Host) {
				return true
			}
		default:
			if host == e.Host {
				return true
			}
		}
	}
	return false
}

func portNumber(port string) uint16 {
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return 0
	}
	return uint16(n)
}
