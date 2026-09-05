package main

import (
	"net/netip"
	"testing"
)

// TestBuildIPBlacklist verifies that buildIPBlacklist parses single IPs and
// CIDR ranges into a matching set, and rejects malformed entries.
func TestBuildIPBlacklist(t *testing.T) {
	t.Run("single IP matches only itself", func(t *testing.T) {
		b, err := buildIPBlacklist([]string{"203.0.113.7"})
		if err != nil {
			t.Fatalf("buildIPBlacklist = %v", err)
		}
		if !b.matches(netip.MustParseAddr("203.0.113.7")) {
			t.Error("203.0.113.7 should match")
		}
		if b.matches(netip.MustParseAddr("203.0.113.8")) {
			t.Error("203.0.113.8 should NOT match a single-IP entry")
		}
	})

	t.Run("CIDR range matches within and outside the block", func(t *testing.T) {
		b, err := buildIPBlacklist([]string{"198.51.100.0/24"})
		if err != nil {
			t.Fatalf("buildIPBlacklist = %v", err)
		}
		if !b.matches(netip.MustParseAddr("198.51.100.123")) {
			t.Error("198.51.100.123 should match 198.51.100.0/24")
		}
		if b.matches(netip.MustParseAddr("198.51.101.5")) { // out of /24
			t.Error("198.51.101.5 should NOT match 198.51.100.0/24")
		}
	})

	t.Run("mixed single IP and CIDR", func(t *testing.T) {
		b, err := buildIPBlacklist([]string{"10.0.0.1", "192.168.0.0/16"})
		if err != nil {
			t.Fatalf("buildIPBlacklist = %v", err)
		}
		if !b.matches(netip.MustParseAddr("10.0.0.1")) ||
			!b.matches(netip.MustParseAddr("192.168.99.99")) {
			t.Error("both entries should match their respective IPs")
		}
	})

	t.Run("IPv4 and IPv6 do not cross-match", func(t *testing.T) {
		b, err := buildIPBlacklist([]string{"10.0.0.0/8", "::1"})
		if err != nil {
			t.Fatalf("buildIPBlacklist = %v", err)
		}
		if !b.matches(netip.MustParseAddr("10.99.0.1")) || !b.matches(netip.MustParseAddr("::1")) {
			t.Error("both entries should match their respective IPs")
		}
		if b.matches(netip.MustParseAddr("::2")) {
			t.Error("::2 should NOT match the single-IPv6 entry ::1")
		}
	})

	t.Run("rejects malformed entry", func(t *testing.T) {
		_, err := buildIPBlacklist([]string{"not-an-ip"})
		if err == nil {
			t.Fatal("expected error for malformed entry")
		}
	})

	t.Run("empty list is empty", func(t *testing.T) {
		b, err := buildIPBlacklist(nil)
		if err != nil {
			t.Fatalf("buildIPBlacklist = %v", err)
		}
		if !b.isEmpty() {
			t.Error("empty blacklist should report isEmpty")
		}
	})
}
