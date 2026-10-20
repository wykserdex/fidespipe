package plugin

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

func perm(host string, port int, anyHost bool) []pipeline.NetworkPermission {
	return []pipeline.NetworkPermission{{Host: host, Port: port, AnyHost: anyHost}}
}

func TestHostAllowed(t *testing.T) {
	cases := []struct {
		name  string
		allow []pipeline.NetworkPermission
		host  string
		port  int
		want  bool
	}{
		{"exact host and port", perm("api.example.com", 443, false), "api.example.com", 443, true},
		{"exact host, other port", perm("api.example.com", 443, false), "api.example.com", 80, false},
		{"foreign host", perm("api.example.com", 443, false), "evil.example.com", 443, false},
		{"suffix is not a match", perm("example.com", 443, false), "notexample.com", 443, false},
		{"case insensitive", perm("API.Example.com", 443, false), "api.example.com", 443, true},
		{"trailing dot normalised", perm("api.example.com", 443, false), "api.example.com.", 443, true},
		{"empty allowlist", nil, "api.example.com", 443, false},
		{"port 0 means any port", perm("api.example.com", 0, false), "api.example.com", 8443, true},
		{"any_host allows all", perm("", 0, true), "anything.example", 1234, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostAllowed(tc.allow, tc.host, tc.port); got != tc.want {
				t.Fatalf("hostAllowed(%q,%d) = %v, want %v", tc.host, tc.port, got, tc.want)
			}
		})
	}
}

func TestMatchHostWildcard(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"*.example.com", "api.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false}, // apex is not covered by a wildcard
		{"*.example.com", "notexample.com", false},
		{"*", "whatever", true},
		{"", "x", false},
	}
	for _, tc := range cases {
		if got := matchHost(tc.pattern, tc.host); got != tc.want {
			t.Errorf("matchHost(%q, %q) = %v, want %v", tc.pattern, tc.host, got, tc.want)
		}
	}
}

// The SSRF guard: a plugin must not reach cloud metadata, loopback or the
// host's private networks even when the hostname is allowlisted. A name that
// resolves into those ranges is a way around a name-based allowlist.
func TestDenyNetwork(t *testing.T) {
	deny := []string{
		"127.0.0.1", "127.1.2.3", // loopback
		"169.254.169.254",                       // cloud instance metadata
		"0.0.0.0",                               // unspecified
		"10.1.2.3", "172.16.0.1", "192.168.1.1", // RFC1918
		"100.64.0.1", // CGNAT
		"224.0.0.1",  // multicast
		"::1",        // IPv6 loopback
		"fe80::1",    // IPv6 link-local
		"fd00::1",    // IPv6 unique-local
	}
	for _, s := range deny {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !denyNetwork(ip) {
			t.Errorf("denyNetwork(%s) = false, want deny", s)
		}
	}
	allow := []string{"93.184.216.34", "1.1.1.1", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, s := range allow {
		if denyNetwork(net.ParseIP(s)) {
			t.Errorf("denyNetwork(%s) = true, want allow", s)
		}
	}
	if !denyNetwork(nil) {
		t.Error("denyNetwork(nil) = false, want deny: a nil address is never dialable")
	}
}

func proxyClient(t *testing.T, e *egress) *http.Client {
	t.Helper()
	u, err := url.Parse(e.addr())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(u)},
		Timeout:   15 * time.Second,
	}
}

// A refusal is a 403, not a transport error: the proxy answers. Asserting on
// err alone would have accepted a proxy that silently let traffic through.
func TestEgressRefusesUndeclaredHostWith403(t *testing.T) {
	e, err := newEgress(perm("api.example.com", 443, false))
	if err != nil {
		t.Fatalf("newEgress: %v", err)
	}
	defer e.stop()

	resp, err := proxyClient(t, e).Get("http://evil.example.com/")
	if err != nil {
		t.Fatalf("request failed at transport level: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("undeclared host got %d, want 403", resp.StatusCode)
	}
}

// CONNECT must stay restricted to TLS ports, otherwise the proxy becomes a
// tunnel to any cleartext protocol.
func TestEgressConnectPortsAreTLSOnly(t *testing.T) {
	e, err := newEgress(perm("api.example.com", 443, false))
	if err != nil {
		t.Fatalf("newEgress: %v", err)
	}
	defer e.stop()
	if !e.tlsPorts[443] {
		t.Error("443 must be permitted for CONNECT")
	}
	if e.tlsPorts[80] {
		t.Error("80 must not be permitted for CONNECT")
	}
	if e.tlsPorts[8080] {
		t.Error("8080 must not be permitted for CONNECT")
	}
}

// env() must emit both cases: clients read one or the other, and a missing one
// silently bypasses the filter.
func TestEgressEnvHasBothCases(t *testing.T) {
	e, err := newEgress(perm("api.example.com", 443, false))
	if err != nil {
		t.Fatalf("newEgress: %v", err)
	}
	defer e.stop()
	joined := strings.Join(e.env(), " ")
	for _, want := range []string{"HTTP_PROXY=", "http_proxy=", "HTTPS_PROXY=", "https_proxy="} {
		if !strings.Contains(joined, want) {
			t.Errorf("environment is missing %s", want)
		}
	}
}

func TestEgressStopIsIdempotentAndClosesListener(t *testing.T) {
	e, err := newEgress(perm("api.example.com", 443, false))
	if err != nil {
		t.Fatalf("newEgress: %v", err)
	}
	addr := strings.TrimPrefix(e.addr(), "http://")
	e.stop()
	e.stop() // a second stop must not panic
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		c.Close()
		t.Fatal("listener must be closed after stop")
	}
}

// An empty allowlist with any_host is a blanket grant; the proxy must then
// accept the declaration as wide and not pretend to filter.
func TestEgressBlanketGrantIsNotNarrowedSilently(t *testing.T) {
	e, err := newEgress(perm("", 0, true))
	if err != nil {
		t.Fatalf("newEgress: %v", err)
	}
	defer e.stop()
	if !hostAllowed(e.allow, "some.random.host", 443) {
		t.Error("any_host must allow any host: the proxy filters only what the manifest does not grant")
	}
	// Even then, private ranges stay denied: any_host is about destinations the
	// plugin talks to, not about reaching the host's internals.
	if !denyNetwork(net.ParseIP("169.254.169.254")) {
		t.Error("metadata address must stay denied under a blanket grant")
	}
}
