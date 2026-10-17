// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func request(remoteAddr, forwarded string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	r.RemoteAddr = remoteAddr
	if forwarded != "" {
		r.Header.Set("X-Forwarded-For", forwarded)
	}
	return r
}

// With nothing configured in front, the socket address is the only thing that
// can be believed - and a header sent by whoever is connecting is exactly what
// must not be.
func TestWithNoProxyConfiguredTheHeaderIsIgnored(t *testing.T) {
	none, err := ParseTrustedProxies(nil)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	got := ClientIP(request("203.0.113.9:44321", "1.2.3.4"), none)
	if got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want the socket address; a spoofed header was believed", got)
	}
}

// The reason this exists: behind nginx every reader arrives from 127.0.0.1,
// and a throttle keyed on that puts the whole household in one bucket.
func TestBehindATrustedProxyTheClientIsRead(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	got := ClientIP(request("127.0.0.1:44321", "198.51.100.4"), trusted)
	if got != "198.51.100.4" {
		t.Errorf("ClientIP = %q, want the reader behind the proxy", got)
	}
}

// The header is read from the RIGHT, past our own hops. Everything to the left
// of the last trusted one is whatever the client chose to send, and believing
// it would let an attacker rotate the throttle key on every attempt.
func TestAForgedPrefixIsNotBelieved(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"127.0.0.1", "10.0.0.0/8"})
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	// The attacker sent "1.2.3.4"; our edge appended their real address, and
	// the inner hop appended the edge's.
	got := ClientIP(request("127.0.0.1:44321", "1.2.3.4, 198.51.100.4, 10.0.0.7"), trusted)
	if got != "198.51.100.4" {
		t.Errorf("ClientIP = %q, want the address our own proxy observed", got)
	}
}

// A request that reaches the port directly is attributed to whoever sent it,
// even when a proxy is configured for other paths in.
func TestADirectRequestIsNotGivenTheProxyTreatment(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	got := ClientIP(request("203.0.113.9:44321", "1.2.3.4"), trusted)
	if got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want the socket address of the direct caller", got)
	}
}

// Degrade to the socket address rather than to no limit at all.
func TestMalformedAndMissingHeadersFallBack(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	for name, forwarded := range map[string]string{
		"absent":         "",
		"not an address": "banana",
		"trailing junk":  "198.51.100.4, banana",
		"all ours":       "127.0.0.1",
	} {
		t.Run(name, func(t *testing.T) {
			got := ClientIP(request("127.0.0.1:44321", forwarded), trusted)
			if got != "127.0.0.1" {
				t.Errorf("ClientIP = %q, want the fallback to the socket address", got)
			}
		})
	}
}

func TestCIDRBlocksAndBareAddressesBothParse(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"10.0.0.0/8", "192.168.1.5", " "})
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if trusted.Empty() {
		t.Fatal("nothing was parsed")
	}
	if !trusted.TrustsAddr("10.4.5.6:100") {
		t.Error("a CIDR block does not match an address inside it")
	}
	if !trusted.TrustsAddr("192.168.1.5:100") {
		t.Error("a bare address does not match itself")
	}
	if trusted.TrustsAddr("203.0.113.9:100") {
		t.Error("an outside address is trusted")
	}
}

func TestAnUnparseableEntryIsRefused(t *testing.T) {
	if _, err := ParseTrustedProxies([]string{"not-an-address"}); err == nil {
		t.Fatal("a nonsense trusted-proxy entry was accepted")
	}
}

// IPv6 arrives bracketed, and the brackets are not part of the address.
func TestIPv6PeersAreMatched(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"::1"})
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if !trusted.TrustsAddr("[::1]:44321") {
		t.Error("a bracketed IPv6 peer was not matched")
	}
	got := ClientIP(request("[::1]:44321", "198.51.100.4"), trusted)
	if got != "198.51.100.4" {
		t.Errorf("ClientIP = %q behind an IPv6 proxy", got)
	}
}
