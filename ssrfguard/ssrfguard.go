// Package ssrfguard provides a shared SSRF egress guard for Pulp extensions.
//
// All Pulp extensions that perform outbound HTTP on behalf of cells (ext-http,
// ext-workers, ext-gin) share identical guard logic: scheme allowlist, resolved-
// IP block at dial time (defeating DNS-rebinding), and per-hop redirect
// re-validation. Only the seed allow-set differs between extensions (ext-gin
// pre-seeds the platform's first-party internal service hostnames so Evolution
// reaches Bananagine/minecraft-resolver out-of-box; ext-http and ext-workers
// use a deny-all-private default).
//
// This package exports the shared logic so the identical copy does not drift
// across three repos. The constructor NewEgressGuard accepts a seedHosts slice
// so callers control the seeding without forking the guard.
package ssrfguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// ErrBlockedTarget is returned by the dialer Control hook when a resolved IP
// falls in a blocked range. It surfaces to the cell as a do-request error,
// same as any other dial failure.
var ErrBlockedTarget = errors.New("ssrf guard: target IP is in a blocked (private/loopback/link-local/metadata) range")

// ErrBlockedScheme is returned when the request URL scheme is not http or https.
var ErrBlockedScheme = errors.New("ssrf guard: only http and https schemes are permitted")

// EgressGuard decides whether an outbound request may proceed. It is
// constructed once per fetcher/pool from the caller-supplied seed hosts and
// the HTTP_FETCH_ALLOW env var.
//
// The guard does three things:
//  1. Scheme allowlist — only http/https (rejects file://, gopher://, …).
//  2. IP block — at DIAL time it validates the RESOLVED IP against a
//     deny-list of loopback / link-local / private / ULA / unspecified
//     ranges. Validating the resolved IP (not the hostname string)
//     defeats DNS-rebinding: even if a name resolves to a public IP at
//     check time and a private IP at connect time, the dialer sees the
//     real connect IP.
//  3. Redirect re-validation — http.Client.CheckRedirect re-runs the
//     scheme check on every hop, and the dialer re-runs the IP check for
//     each hop's connection, so a redirect to an internal target is
//     refused mid-chain.
//
// The name-allowlist exemption is decided PER DIAL against the host the
// dialer is actually about to connect to, NOT pinned once onto the request
// context. This matters for redirects: an allowlisted host that 302s to a
// loopback / metadata / RFC-1918 target is still IP-blocked, because the
// redirect hop dials a DIFFERENT host that is re-checked against the
// allowlist on its own.
type EgressGuard struct {
	// allowHosts is a set of explicitly-permitted host strings
	// (lower-cased, host or host:port) that bypass the IP block.
	allowHosts map[string]struct{}
	// allowNets is a list of explicitly-permitted CIDRs whose IPs bypass
	// the IP block (for a known internal service range).
	allowNets []*net.IPNet
}

// NewEgressGuard builds an EgressGuard. seedHosts are seeded into the
// allow-set first (exact lower-cased hostnames; nil is safe). Then allowList
// (comma-separated host[:port] or CIDR entries) is parsed and merged on top.
// Whitespace and empty entries are ignored. Malformed entries are skipped.
//
// Call sites:
//   - ext-gin: NewEgressGuard(allowList, defaultInternalServiceHosts) — seeds
//     the platform's first-party Docker-bridge hostnames so Evolution can
//     reach Bananagine/minecraft-resolver out-of-box.
//   - ext-http, ext-workers: NewEgressGuard(allowList, nil) — deny-all-private
//     default; only HTTP_FETCH_ALLOW-supplied entries are permitted.
func NewEgressGuard(allowList string, seedHosts []string) *EgressGuard {
	g := &EgressGuard{allowHosts: map[string]struct{}{}}
	// Seed caller-supplied hosts first (e.g. ext-gin's first-party names).
	for _, h := range seedHosts {
		g.allowHosts[strings.ToLower(h)] = struct{}{}
	}
	// Parse the operator-supplied allowList on top.
	for _, raw := range strings.Split(allowList, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, ipnet, err := net.ParseCIDR(entry); err == nil {
				g.allowNets = append(g.allowNets, ipnet)
			}
			continue
		}
		g.allowHosts[strings.ToLower(entry)] = struct{}{}
	}
	return g
}

// HostAllowed reports whether host (the URL host, optionally host:port) is
// on the explicit allowlist and therefore exempt from the IP block.
func (g *EgressGuard) HostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if _, ok := g.allowHosts[host]; ok {
		return true
	}
	// Also match the bare hostname when the allow entry omitted the port.
	if h, _, err := net.SplitHostPort(host); err == nil {
		if _, ok := g.allowHosts[h]; ok {
			return true
		}
	}
	return false
}

// IPAllowed reports whether ip is inside one of the explicitly-allowed
// CIDRs, exempting it from the block check.
func (g *EgressGuard) IPAllowed(ip net.IP) bool {
	for _, n := range g.allowNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// IPBlocked reports whether ip falls in a range a cell must not reach.
// Covers loopback, link-local (incl. 169.254.169.254 cloud metadata),
// RFC-1918 / ULA private, unspecified, and other non-global-unicast
// addresses (multicast, etc.).
func IPBlocked(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		ip.IsPrivate() { // RFC-1918 + ULA (fc00::/7)
		return true
	}
	return false
}

// DialControl is the net.Dialer.Control hook. It runs AFTER name resolution,
// once per resolved address the dialer attempts, with the concrete IP:port it
// is about to connect to. Returning an error aborts that connection — so
// DNS-rebinding (resolve-to-public, connect-to-private) cannot slip past,
// because we check the address actually dialed.
//
// Control's signature has no context, so the name-allowlist exemption is
// handled separately in DialContext (which sees the dialed host:port); this
// hook only performs the unconditional IP block.
func (g *EgressGuard) DialControl(_ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ErrBlockedTarget
	}
	if g.IPAllowed(ip) {
		return nil
	}
	if IPBlocked(ip) {
		return fmt.Errorf("%w: %s", ErrBlockedTarget, ip.String())
	}
	return nil
}

// DialContext wraps net.Dialer.DialContext and decides the name-allowlist
// exemption PER DIAL, keyed on the host:port the dialer is actually about
// to connect to (`address`). http.Transport passes the CURRENT hop's
// unresolved host:port here — so on a redirect, this runs again with the
// REDIRECT TARGET's host, not the original request's. A host that is on
// the name allowlist bypasses the IP block (it may legitimately resolve to
// a private IP, e.g. an internal service reached by name); any other host
// — including a redirect target that is NOT allowlisted — goes through the
// Control-guarded base dialer and is IP-blocked if it lands on a
// loopback / metadata / RFC-1918 address.
//
// This is what closes the redirect-bypass: the exemption is never pinned
// to the request context, so it cannot ride a 302 from an allowlisted host
// to an internal target. Each hop earns (or is denied) its own exemption.
func (g *EgressGuard) DialContext(base func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	exempt := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if g.HostAllowed(address) {
			return exempt.DialContext(ctx, network, address)
		}
		return base(ctx, network, address)
	}
}

// CheckScheme validates the request URL's scheme. It is run for the
// initial request and re-run for every redirect hop via
// http.Client.CheckRedirect.
func (g *EgressGuard) CheckScheme(req *http.Request) error {
	switch strings.ToLower(req.URL.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("%w: got %q", ErrBlockedScheme, req.URL.Scheme)
	}
}

// Prepare validates the request's scheme before client.Do. The
// name-allowlist / IP-block decision is NOT made here — it is made per dial
// in DialContext/DialControl against the host actually being connected to,
// so it re-evaluates correctly on every redirect hop. Scheme is re-checked
// on redirects by CheckRedirect.
func (g *EgressGuard) Prepare(req *http.Request) (*http.Request, error) {
	if err := g.CheckScheme(req); err != nil {
		return nil, err
	}
	return req, nil
}
