// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// The policy as a table of messy inputs: the edges of every range, the forms an
// address can hide in, and allowlist entries against near misses.
func TestPolicyDecidesOnTheResolvedAddress(t *testing.T) {
	allowed, err := ParseAllowList(" kb.example.net, *.example.com ,93.184.216.34, pay.example.org:8443,, [2001:db8::7]:443 ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	policy := NewPolicy(allowed, "backend.example.io:9000")

	for _, tc := range []struct {
		name, host, port, addr string
		want                   Rule
		isAllowed              bool
	}{
		// 172.16.0.0/12, at both ends and one past each.
		{"just below 172.16/12", "172.15.255.255", "80", "172.15.255.255", RulePublicNotListed, false},
		{"first of 172.16/12", "172.16.0.0", "80", "172.16.0.0", RulePrivate, true},
		{"last of 172.16/12", "172.31.255.255", "80", "172.31.255.255", RulePrivate, true},
		{"just above 172.16/12", "172.32.0.0", "80", "172.32.0.0", RulePublicNotListed, false},
		{"10/8", "10.1.2.3", "80", "10.1.2.3", RulePrivate, true},
		{"192.168/16", "192.168.31.55", "8770", "192.168.31.55", RulePrivate, true},
		{"loopback other than .1", "127.0.0.2", "80", "127.0.0.2", RuleLoopback, true},
		{"IPv6 loopback", "::1", "80", "::1", RuleLoopback, true},
		{"unique local IPv6", "fd12::1", "80", "fd12::1", RulePrivate, true},
		{"IPv4-mapped private", "::ffff:10.0.0.1", "80", "::ffff:10.0.0.1", RulePrivate, true},
		{"IPv4-mapped metadata", "::ffff:169.254.169.254", "80", "::ffff:169.254.169.254", RuleLinkLocal, false},
		{"NAT64 metadata", "64:ff9b::a9fe:a9fe", "80", "64:ff9b::a9fe:a9fe", RuleLinkLocal, false},
		{"metadata unlisted", "169.254.169.254", "80", "169.254.169.254", RuleLinkLocal, false},
		{"IPv6 link-local", "fe80::1", "80", "fe80::1", RuleLinkLocal, false},
		{"unspecified IPv4", "0.0.0.0", "80", "0.0.0.0", RuleUnspecified, false},
		{"this-network IPv4", "0.1.2.3", "80", "0.1.2.3", RuleUnspecified, false},
		{"unspecified IPv6", "::", "80", "::", RuleUnspecified, false},
		{"multicast", "239.1.1.1", "80", "239.1.1.1", RuleMulticast, false},
		{"broadcast", "255.255.255.255", "80", "255.255.255.255", RuleMulticast, false},

		// The allowlist.
		{"public IP literal listed", "93.184.216.34", "443", "93.184.216.34", RuleAllowedHost, true},
		{"public IP literal unlisted", "93.184.216.35", "443", "93.184.216.35", RulePublicNotListed, false},
		{"listed name resolving public", "kb.example.net", "443", "203.0.113.9", RuleAllowedHost, true},
		{"listed name, upper case, trailing dot", "KB.Example.NET.", "443", "203.0.113.9", RuleAllowedHost, true},
		{"unlisted name resolving public", "kb.example.biz", "443", "203.0.113.9", RulePublicNotListed, false},
		{"wildcard does not match the bare domain", "example.com", "443", "203.0.113.9", RulePublicNotListed, false},
		{"wildcard matches a subdomain", "api.example.com", "443", "203.0.113.9", RuleAllowedHost, true},
		{"wildcard matches a deeper subdomain", "a.b.example.com", "443", "203.0.113.9", RuleAllowedHost, true},
		{"wildcard does not match a lookalike", "badexample.com", "443", "203.0.113.9", RulePublicNotListed, false},
		{"host:port entry on its port", "pay.example.org", "8443", "203.0.113.9", RuleAllowedHost, true},
		{"host:port entry on another port", "pay.example.org", "443", "203.0.113.9", RulePublicNotListed, false},
		{"bracketed IPv6 entry on its port", "2001:db8::7", "443", "2001:db8::7", RuleAllowedHost, true},
		{"bracketed IPv6 entry on another port", "2001:db8::7", "80", "2001:db8::7", RulePublicNotListed, false},
		{"a listed name does not list its address", "203.0.113.9", "443", "203.0.113.9", RulePublicNotListed, false},
		{"the backend base host on any port", "backend.example.io", "443", "198.51.100.1", RuleAllowedHost, true},
		// Listing never opens what is always refused.
		{"listed name resolving to metadata", "kb.example.net", "443", "169.254.169.254", RuleLinkLocal, false},
		{"wildcard name resolving to unspecified", "api.example.com", "443", "0.0.0.0", RuleUnspecified, false},
		// A private address needs no listing, whatever the name.
		{"unlisted name resolving private", "kb.internal", "8080", "10.20.30.40", RulePrivate, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule, err := policy.Check(tc.host, tc.port, netip.MustParseAddr(tc.addr))
			if rule != tc.want {
				t.Errorf("rule = %s, want %s", rule, tc.want)
			}
			if (err == nil) != tc.isAllowed {
				t.Fatalf("err = %v, want allowed=%v", err, tc.isAllowed)
			}
			if err != nil {
				refusal, ok := IsRefusal(err)
				if !ok {
					t.Fatalf("err %T is not a *Refusal", err)
				}
				if refusal.Host != tc.host || refusal.Rule != tc.want || !strings.Contains(refusal.Address, strings.Trim(netip.MustParseAddr(tc.addr).String(), "[]")) {
					t.Errorf("refusal = %+v, want host %s, address %s, rule %s", refusal, tc.host, tc.addr, tc.want)
				}
			}
		})
	}
}

func TestParseAllowListRefusesWhatItCannotMean(t *testing.T) {
	for _, raw := range []string{
		"*",
		"*.com",
		"kb.example.com:0",
		"kb.example.com:99999",
		"kb.example.com:https",
		"kb example.com",
		"http://kb.example.com",
		"kb.example.com/path",
		"fe80::1%eth0",
		"2130706433",
		"0x7f.1",
		"2001:db8::1:x",
		"-bad.example.com",
	} {
		if _, err := ParseAllowList(raw); err == nil {
			t.Errorf("%q was accepted", raw)
		}
	}
	entries, err := ParseAllowList("2001:db8::1, [2001:db8::2]:443, Kb.Example.com., *.Example.org:8443")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.String())
	}
	if want := "2001:db8::1|[2001:db8::2]:443|kb.example.com|*.example.org:8443"; strings.Join(got, "|") != want {
		t.Errorf("entries = %s, want %s", strings.Join(got, "|"), want)
	}
}

// fakeNet is a resolver and a connector with no network behind them: every
// allowed connection goes to one local listener, and each one is counted.
type fakeNet struct {
	mu       sync.Mutex
	answers  map[string][][]netip.Addr // per host, one answer per lookup
	lookups  map[string]int
	connects atomic.Int32
	listener net.Listener
}

func newFakeNet(t *testing.T) *fakeNet {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	return &fakeNet{answers: map[string][][]netip.Addr{}, lookups: map[string]int{}, listener: ln}
}

func (f *fakeNet) answer(host string, addrs ...string) {
	var parsed []netip.Addr
	for _, a := range addrs {
		parsed = append(parsed, netip.MustParseAddr(a))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[host] = append(f.answers[host], parsed)
}

func (f *fakeNet) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	answers := f.answers[host]
	if len(answers) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	i := min(f.lookups[host], len(answers)-1)
	f.lookups[host]++
	return answers[i], nil
}

func (f *fakeNet) connect(ctx context.Context, network, _ string) (net.Conn, error) {
	f.connects.Add(1)
	var d net.Dialer
	return d.DialContext(ctx, network, f.listener.Addr().String())
}

func (f *fakeNet) dialer(policy *Policy) *Dialer {
	return &Dialer{Policy: policy, Resolver: f.resolve, Connect: f.connect}
}

// A name that resolves to a private address and then, on the next lookup, to
// the metadata address is let through once and refused the second time,
// before any connection is opened.
func TestARebindingNameIsRefusedAtConnect(t *testing.T) {
	f := newFakeNet(t)
	f.answer("kb.internal", "10.0.0.5")
	f.answer("kb.internal", "169.254.169.254")
	d := f.dialer(NewPolicy(nil))

	conn, err := d.DialContext(context.Background(), "tcp", "kb.internal:80")
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	conn.Close()
	if got := f.connects.Load(); got != 1 {
		t.Fatalf("connections after the first dial = %d, want 1", got)
	}

	_, err = d.DialContext(context.Background(), "tcp", "kb.internal:80")
	refusal, ok := IsRefusal(err)
	if !ok || refusal.Rule != RuleLinkLocal || refusal.Address != "169.254.169.254:80" || refusal.Host != "kb.internal" {
		t.Fatalf("second dial: err = %v, want a link-local refusal naming host and address", err)
	}
	if got := f.connects.Load(); got != 1 {
		t.Errorf("connections after the refusal = %d, want still 1", got)
	}
}

// Every refusal is decided before a connection exists.
func TestEveryRefusalOpensNoConnection(t *testing.T) {
	f := newFakeNet(t)
	f.answer("public.example.com", "203.0.113.10")
	f.answer("listed.example.com", "203.0.113.11")
	f.answer("mixed.example.com", "169.254.169.254", "0.0.0.0")
	allowed, _ := ParseAllowList("listed.example.com:443")
	d := f.dialer(NewPolicy(allowed))

	for _, tc := range []struct {
		address string
		rule    Rule
	}{
		{"169.254.169.254:80", RuleLinkLocal},
		{"[::ffff:169.254.169.254]:80", RuleLinkLocal},
		{"[fe80::1]:80", RuleLinkLocal},
		{"[fe80::1%25en0]:80", RuleLinkLocal},
		{"0.0.0.0:80", RuleUnspecified},
		{"[::]:80", RuleUnspecified},
		{"2130706433:80", RuleNonCanonical},
		{"0x7f.1:80", RuleNonCanonical},
		{"017.0.0.1:80", RuleNonCanonical},
		{"public.example.com:443", RulePublicNotListed},
		{"listed.example.com:80", RulePublicNotListed},
		{"8.8.8.8:53", RulePublicNotListed},
		{"mixed.example.com:80", RuleLinkLocal},
	} {
		_, err := d.DialContext(context.Background(), "tcp", tc.address)
		refusal, ok := IsRefusal(err)
		if !ok {
			t.Errorf("%s: err = %v, want a refusal", tc.address, err)
			continue
		}
		if refusal.Rule != tc.rule {
			t.Errorf("%s: rule = %s, want %s", tc.address, refusal.Rule, tc.rule)
		}
	}
	if got := f.connects.Load(); got != 0 {
		t.Errorf("%d connections were opened, want none", got)
	}

	// The listed host on its listed port does connect, through the same seam.
	conn, err := d.DialContext(context.Background(), "tcp", "listed.example.com:443")
	if err != nil {
		t.Fatalf("listed host: %v", err)
	}
	conn.Close()
	if got := f.connects.Load(); got != 1 {
		t.Errorf("connections = %d, want 1", got)
	}
}

// The production path: no Connect seam, so the socket's Control hook judges
// the address. A loopback listener is reached; the hook itself refuses what
// the policy refuses, with or without the host the dial was asked for.
func TestTheControlHookJudgesTheSocketAddress(t *testing.T) {
	f := newFakeNet(t)
	f.answer("kb.internal", "127.0.0.1")
	d := &Dialer{Policy: NewPolicy(nil), Resolver: f.resolve}

	_, port, _ := net.SplitHostPort(f.listener.Addr().String())
	conn, err := d.DialContext(context.Background(), "tcp", net.JoinHostPort("kb.internal", port))
	if err != nil {
		t.Fatalf("dial a loopback backend: %v", err)
	}
	conn.Close()

	withTarget := context.WithValue(context.Background(), targetKey{}, target{host: "kb.example.net", port: "80"})
	for _, tc := range []struct {
		ctx     context.Context
		address string
		isOK    bool
	}{
		{withTarget, "169.254.169.254:80", false},
		{withTarget, "[::ffff:169.254.169.254]:80", false},
		{withTarget, "203.0.113.9:80", false},
		{withTarget, "10.0.0.1:80", true},
		{context.Background(), "203.0.113.9:80", false},
		{context.Background(), "127.0.0.1:80", true},
	} {
		err := d.control(tc.ctx, "tcp4", tc.address, nil)
		if (err == nil) != tc.isOK {
			t.Errorf("control(%s) = %v, want ok=%v", tc.address, err, tc.isOK)
		}
	}

	if err := d.control(context.Background(), "tcp4", "not-an-address", nil); err == nil {
		t.Error("an unreadable address passed the hook")
	}
}

func TestRefusalMessagesNameHostAddressAndReason(t *testing.T) {
	err := error(&Refusal{Host: "kb.example.biz", Address: "203.0.113.9:443", Rule: RulePublicNotListed})
	for _, want := range []string{"kb.example.biz", "203.0.113.9:443", "AICC_TOOL_ALLOWED_HOSTS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q does not mention %q", err, want)
		}
	}
	if _, ok := IsRefusal(errors.Join(errors.New("other"), err)); !ok {
		t.Error("a wrapped refusal was not found")
	}
}
