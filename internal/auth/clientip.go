// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"net"
	"net/http"
	"strings"
)

// TrustedProxies is the set of addresses that are this installation's own
// reverse proxies.
//
// It answers one question - "did this request come from my own front door?" -
// and two things depend on the answer: whether an identity header may be
// believed, and whose address a sign-in attempt should be counted against.
//
// The second is why this exists separately from the proxy sign-in provider.
// Behind nginx or Caddy every request arrives from 127.0.0.1, so a throttle
// keyed on the socket address puts every reader in the household into one
// bucket: eight wrong passwords from anybody would lock sign-in for everyone.
// A rate limit that punishes the victim is worse than none, because it looks
// like protection.
type TrustedProxies struct {
	networks []*net.IPNet
}

// ParseTrustedProxies reads a list of addresses and CIDR blocks.
//
// A bare address becomes a single-host network, because "10.0.0.5" is what
// somebody writes when they mean their proxy and not a range.
func ParseTrustedProxies(entries []string) (*TrustedProxies, error) {
	t := &TrustedProxies{}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if _, network, err := net.ParseCIDR(entry); err == nil {
			t.networks = append(t.networks, network)
			continue
		}
		ip := net.ParseIP(entry)
		if ip == nil {
			return nil, errors.New(entry + " is not an IP address or CIDR block")
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		t.networks = append(t.networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return t, nil
}

// Empty reports whether nothing is trusted, which is the default and means
// every request is taken at its socket address.
func (t *TrustedProxies) Empty() bool { return t == nil || len(t.networks) == 0 }

// Contains reports whether an address is one of the configured proxies.
func (t *TrustedProxies) Contains(ip net.IP) bool {
	if t == nil || ip == nil {
		return false
	}
	for _, network := range t.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// TrustsAddr reports whether a host:port peer address is a trusted proxy.
func (t *TrustedProxies) TrustsAddr(remoteAddr string) bool {
	return t.Contains(hostIP(remoteAddr))
}

// ClientIP is the address a request should be attributed to.
//
// The socket address, unless the peer is one of this installation's own
// proxies - in which case X-Forwarded-For is read from the RIGHT, skipping
// further trusted hops, and the first address that is not one of ours is the
// client. Reading from the right is what makes the header unforgeable: a
// client can put anything at the front of X-Forwarded-For, and everything to
// the left of the last trusted hop is exactly that - whatever the client
// claimed. Only the entries appended by our own proxies can be believed.
//
// Falls back to the socket address whenever the header is absent, unparseable,
// or entirely trusted addresses, so a misconfiguration degrades to the old
// behaviour rather than to "no limit at all".
func ClientIP(r *http.Request, trusted *TrustedProxies) string {
	peer := hostIP(r.RemoteAddr)
	if peer == nil {
		return r.RemoteAddr
	}
	if !trusted.Contains(peer) {
		return peer.String()
	}

	forwarded := r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return peer.String()
	}
	hops := strings.Split(forwarded, ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.Trim(strings.TrimSpace(hops[i]), "[]"))
		if ip == nil {
			// A malformed entry means the chain cannot be walked past this
			// point with any confidence. Stop rather than guess.
			break
		}
		if !trusted.Contains(ip) {
			return ip.String()
		}
	}
	return peer.String()
}

// hostIP pulls the IP out of a host:port address, tolerating one without a
// port and the brackets IPv6 carries.
func hostIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}
