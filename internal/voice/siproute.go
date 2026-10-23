// SPDX-License-Identifier: Apache-2.0

package voice

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// sipHop is where a SIP URI sends a request: the address to put it on the wire
// to, and what the URI says about how to get there.
type sipHop struct {
	scheme string // "sip" or "sips", lowercased
	// host has no IPv6 brackets. It is the maddr parameter when the URI
	// carries one, which overrides the host part as the address to contact
	// (RFC 3261 §19.1.1).
	host string
	// port is the URI's, or the scheme's default: 5060 for sip, 5061 for sips.
	port int
	// transport is the transport parameter, lowercased; empty when absent.
	transport string
	// isLooseRouter is the lr parameter. A route entry without it names a
	// strict router (RFC 3261 §12.2.1.1).
	isLooseRouter bool
}

// parseHop reads the parts of a sip: or sips: URI that decide where a request
// goes. The user part and any ?headers are ignored; an IPv6 host must be
// bracketed, as the URI grammar requires.
func parseHop(uri string) (sipHop, error) {
	scheme, rest, ok := strings.Cut(uri, ":")
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if !ok || (scheme != "sip" && scheme != "sips") {
		return sipHop{}, fmt.Errorf("%q is not a sip: or sips: URI", uri)
	}
	hop := sipHop{scheme: scheme}

	rest, _, _ = strings.Cut(rest, "?")
	// A parameter cannot contain an unescaped '@', so the last one ends the
	// user part even when the user part carries parameters of its own.
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	hostport, params, _ := strings.Cut(rest, ";")

	host, port := hostport, ""
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return sipHop{}, fmt.Errorf("%q has an unterminated IPv6 reference", uri)
		}
		host = hostport[1:end]
		if after := hostport[end+1:]; after != "" {
			var found bool
			if port, found = strings.CutPrefix(after, ":"); !found {
				return sipHop{}, fmt.Errorf("%q has text after its IPv6 reference", uri)
			}
		}
	} else if h, p, found := strings.Cut(hostport, ":"); found {
		if strings.Contains(p, ":") {
			return sipHop{}, fmt.Errorf("%q has an IPv6 host without brackets", uri)
		}
		host, port = h, p
	}
	if host == "" {
		return sipHop{}, fmt.Errorf("%q names no host", uri)
	}
	hop.host = host
	switch {
	case port != "":
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return sipHop{}, fmt.Errorf("%q has an invalid port %q", uri, port)
		}
		hop.port = n
	case scheme == "sips":
		hop.port = 5061
	default:
		hop.port = 5060
	}

	for param := range strings.SplitSeq(params, ";") {
		name, value, _ := strings.Cut(param, "=")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "lr":
			hop.isLooseRouter = true
		case "transport":
			hop.transport = strings.ToLower(strings.TrimSpace(value))
		case "maddr":
			if maddr := strings.Trim(strings.TrimSpace(value), "[]"); maddr != "" {
				hop.host = maddr
			}
		}
	}
	return hop, nil
}

// udpAddr is the UDP address a request to the hop is written to.
//
// network is the socket's: "udp4" for an IPv4-only socket, which the UAS
// binds, or "udp". Only an IP literal is accepted; a hostname is refused
// rather than looked up. The switch this process talks to is on the LAN and
// names itself by address, so a name here is not something to wait on DNS
// for. A sips: URI, a transport other than udp, and an IPv6 address on an
// IPv4 socket are refused too, rather than sent somewhere they cannot be
// understood or cannot go.
func (h sipHop) udpAddr(network string) (*net.UDPAddr, error) {
	if h.scheme == "sips" {
		return nil, errors.New("sips: needs TLS, and SIP here is UDP only")
	}
	if h.transport != "" && h.transport != "udp" {
		return nil, fmt.Errorf("transport=%s, and SIP here is UDP only", h.transport)
	}
	addr, err := netip.ParseAddr(h.host)
	if err != nil {
		return nil, fmt.Errorf("%s is a hostname, and names are not resolved", h.host)
	}
	addr = addr.Unmap()
	if addr.IsUnspecified() {
		return nil, fmt.Errorf("%s is not an address a request can be sent to", addr)
	}
	if network == "udp4" && !addr.Is4() {
		return nil, fmt.Errorf("%s is IPv6 and the SIP socket is IPv4 only", addr)
	}
	return net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, uint16(h.port))), nil
}

// routeSetNextHop is where an in-dialog request goes when the dialog has a
// route set: the first entry's URI, whether that entry is a loose router (it
// is the top Route) or a strict one (it is the Request-URI). RFC 3261 §8.1.2
// sends the request there, not to the remote target.
func routeSetNextHop(routeSet []string, network string) (*net.UDPAddr, error) {
	if len(routeSet) == 0 {
		return nil, errors.New("the route set is empty")
	}
	uri, ok := findURI(routeSet[0])
	if !ok {
		return nil, fmt.Errorf("%q carries no sip: or sips: URI", routeSet[0])
	}
	hop, err := parseHop(uri)
	if err != nil {
		return nil, err
	}
	return hop.udpAddr(network)
}

// dialogRouting is how an in-dialog request is addressed.
type dialogRouting struct {
	requestURI string
	// routes are the Route header values, in order.
	routes []string
}

// routeRequest addresses an in-dialog request as RFC 3261 §12.2.1.1 does.
//
// An empty route set, or one whose first entry is a loose router (carries
// `;lr`), puts the remote target in the Request-URI and the whole set, in
// order, in Route headers. A first entry without `;lr` is a strict router,
// which reads the Request-URI to route: that entry's URI becomes the
// Request-URI, the rest of the set follows in order, and the remote target is
// appended as the last Route so the request still reaches the far end. A
// strict entry further down the set changes nothing here; the loose router
// before it does the rewrite.
func routeRequest(routeSet []string, remoteTarget string) dialogRouting {
	if len(routeSet) > 0 {
		if uri, ok := findURI(routeSet[0]); ok {
			if hop, err := parseHop(uri); err == nil && !hop.isLooseRouter {
				routes := make([]string, 0, len(routeSet))
				routes = append(routes, routeSet[1:]...)
				routes = append(routes, "<"+remoteTarget+">")
				return dialogRouting{requestURI: requestURIForm(uri), routes: routes}
			}
		}
	}
	return dialogRouting{requestURI: requestURIForm(remoteTarget), routes: routeSet}
}

// requestURIForm drops what a URI may carry but a Request-URI may not
// (RFC 3261 §19.1.1): the ?headers part and the method parameter.
func requestURIForm(uri string) string {
	uri, _, _ = strings.Cut(uri, "?")
	if !strings.Contains(strings.ToLower(uri), ";method=") {
		return uri
	}
	parts := strings.Split(uri, ";")
	kept := []string{parts[0]}
	for _, p := range parts[1:] {
		name, _, _ := strings.Cut(p, "=")
		if !strings.EqualFold(strings.TrimSpace(name), "method") {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ";")
}
