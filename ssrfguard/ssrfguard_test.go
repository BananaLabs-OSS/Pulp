package ssrfguard

import (
	"net"
	"testing"
)

// TestIPBlocked_Ranges confirms IPBlocked correctly classifies private,
// loopback, link-local, metadata, ULA, and unspecified ranges as blocked,
// and leaves public unicast addresses unblocked.
func TestIPBlocked_Ranges(t *testing.T) {
	blocked := []string{
		"127.0.0.1",       // loopback
		"::1",             // loopback v6
		"169.254.169.254", // cloud metadata (link-local)
		"10.1.2.3",        // RFC-1918
		"172.16.0.1",      // RFC-1918
		"192.168.1.1",     // RFC-1918
		"fc00::1",         // ULA
		"0.0.0.0",         // unspecified
	}
	for _, s := range blocked {
		if !IPBlocked(net.ParseIP(s)) {
			t.Errorf("IPBlocked(%s) = false, want true", s)
		}
	}
	public := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range public {
		if IPBlocked(net.ParseIP(s)) {
			t.Errorf("IPBlocked(%s) = true, want false (public)", s)
		}
	}
}

// TestNewEgressGuard_DenyAllPrivate confirms that with no seed hosts and no
// allowList, the guard's HostAllowed returns false for arbitrary hosts and
// IPBlocked remains unconditional.
func TestNewEgressGuard_DenyAllPrivate(t *testing.T) {
	g := NewEgressGuard("", nil)
	if g.HostAllowed("internal-service") {
		t.Fatal("deny-all guard should not allow any host")
	}
	if g.HostAllowed("bananagine") {
		t.Fatal("deny-all guard should not allow bananagine (no seed)")
	}
}

// TestNewEgressGuard_SeedHosts confirms that seedHosts are pre-seeded into
// the allow-set, making them exempt from the IP block.
func TestNewEgressGuard_SeedHosts(t *testing.T) {
	seeds := []string{"bananagine", "bananagine:3000", "minecraft-resolver", "minecraft-resolver:8080"}
	g := NewEgressGuard("", seeds)
	for _, h := range seeds {
		if !g.HostAllowed(h) {
			t.Errorf("seeded host %q should be allowed", h)
		}
	}
	// A name not in the seed list must not be allowed.
	if g.HostAllowed("evil-internal") {
		t.Fatal("non-seeded host must not be allowed")
	}
}

// TestNewEgressGuard_AllowListCIDR confirms that CIDR entries in allowList
// are parsed and stored in allowNets so IPAllowed returns true for IPs in range.
func TestNewEgressGuard_AllowListCIDR(t *testing.T) {
	g := NewEgressGuard("127.0.0.0/8", nil)
	if !g.IPAllowed(net.ParseIP("127.0.0.1")) {
		t.Fatal("127.0.0.1 should be allowed by 127.0.0.0/8 CIDR")
	}
	if g.IPAllowed(net.ParseIP("192.168.1.1")) {
		t.Fatal("192.168.1.1 should not be allowed by 127.0.0.0/8")
	}
}

// TestNewEgressGuard_AllowListHost confirms that literal host entries in
// allowList are stored in allowHosts (case-insensitive).
func TestNewEgressGuard_AllowListHost(t *testing.T) {
	g := NewEgressGuard("MyService:8080", nil)
	if !g.HostAllowed("myservice:8080") {
		t.Fatal("allowlisted host should be allowed (lower-cased)")
	}
	if !g.HostAllowed("MyService:8080") {
		t.Fatal("allowlisted host should be allowed (original case)")
	}
}

// TestHostAllowed_BareHostMatchesWithPort confirms that an allowlist entry
// with no port (bare hostname) still matches when the dialed address includes
// a port.
func TestHostAllowed_BareHostMatchesWithPort(t *testing.T) {
	g := NewEgressGuard("myservice", nil)
	if !g.HostAllowed("myservice:9000") {
		t.Fatal("bare-hostname allowlist entry should match host:port form")
	}
}

// TestIPBlocked_NilIP confirms that a nil IP is treated as blocked (defensive).
func TestIPBlocked_NilIP(t *testing.T) {
	if !IPBlocked(nil) {
		t.Fatal("nil IP should be blocked")
	}
}
