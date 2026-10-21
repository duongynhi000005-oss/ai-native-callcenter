// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// Dialer opens connections under a Policy. It is meant to be an
// http.Transport's DialContext.
//
// It resolves the host itself, checks every address it got, and connects only
// to an address that passed, by that address: nothing is resolved twice, so
// the address judged is the address reached. The connection's own Control hook
// checks the address once more as the socket is about to connect, which is
// where the decision finally belongs (issue #286): whatever path a dial takes,
// no socket connects to an address the policy refuses.
type Dialer struct {
	Policy *Policy
	// Resolver looks a host name up. Nil uses net.DefaultResolver. Tests
	// replace it so that a name can resolve to any address without DNS.
	Resolver func(ctx context.Context, host string) ([]netip.Addr, error)
	// Connect opens the connection to an address the policy allowed. Nil uses
	// a net.Dialer whose Control hook re-checks the address. Tests replace it
	// to route an allowed public address to a local listener.
	Connect func(ctx context.Context, network, address string) (net.Conn, error)
	// Timeout bounds one connection attempt; the request's context bounds the
	// whole dial. Zero means 10 s.
	Timeout time.Duration
}

// targetKey carries the host a dial was asked for into the Control hook,
// which otherwise sees only the address.
type targetKey struct{}

type target struct{ host, port string }

// DialContext connects to address ("host:port") if the policy allows it.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if isNonCanonicalNumeric(host) {
		return nil, &Refusal{Host: host, Rule: RuleNonCanonical}
	}

	addrs, err := d.resolve(ctx, host)
	if err != nil {
		return nil, err
	}

	ctx = context.WithValue(ctx, targetKey{}, target{host: host, port: port})
	var errs []error
	for _, addr := range addrs {
		if _, err := d.Policy.Check(host, port, addr); err != nil {
			errs = append(errs, err)
			continue
		}
		conn, err := d.connect(ctx, network, netip.AddrPortFrom(addr.WithZone(""), portNumber(port)).String())
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("%s resolved to no address", host)
	}
	// The first error is the one worth reading: for a name that resolved to
	// one address, it is the only one.
	if len(errs) == 1 {
		return nil, errs[0]
	}
	return nil, errors.Join(errs...)
}

// resolve turns host into the addresses to try. An IP literal is itself.
func (d *Dialer) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			// A zone names an interface, which only a link-local address
			// needs; refuse it outright rather than parse what it means.
			return nil, &Refusal{Host: host, Rule: RuleLinkLocal}
		}
		return []netip.Addr{addr}, nil
	}
	if d.Resolver != nil {
		return d.Resolver(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (d *Dialer) connect(ctx context.Context, network, address string) (net.Conn, error) {
	if d.Connect != nil {
		return d.Connect(ctx, network, address)
	}
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout, ControlContext: d.control}
	return dialer.DialContext(ctx, network, address)
}

// control runs as a socket is about to connect, on the address it will connect
// to. A dial that did not come through DialContext (and so carries no host) is
// judged as if the address were the host, which only an allowlisted IP literal
// can pass when the address is public.
func (d *Dialer) control(ctx context.Context, _, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("outbound host policy: cannot read the address %q", address)
	}
	t, ok := ctx.Value(targetKey{}).(target)
	if !ok {
		t = target{host: addrPort.Addr().String(), port: fmt.Sprint(addrPort.Port())}
	}
	_, err = d.Policy.Check(t.host, t.port, addrPort.Addr())
	return err
}
