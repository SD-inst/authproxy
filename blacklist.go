package main

import (
	"fmt"
	"log"
	"net/netip"

	"github.com/labstack/echo/v4"
)

// ipBlacklist is the parsed ip_blacklist from config: a set of prefixes (bare
// IPs stored as /32 or /128, and CIDR ranges as-is) for membership checks.
type ipBlacklist struct {
	prefixes []netip.Prefix
}

// buildIPBlacklist parses config.IPBlacklist entries into an ipBlacklist. Each
// entry is either a bare IP (stored as an exact /32 or /128 prefix) or a CIDR
// range. A malformed entry is a hard error so a typo cannot silently disable
// the blacklist.
func buildIPBlacklist(entries []string) (*ipBlacklist, error) {
	b := &ipBlacklist{prefixes: make([]netip.Prefix, 0, len(entries))}
	for _, e := range entries {
		if addr, err := netip.ParseAddr(e); err == nil {
			b.prefixes = append(b.prefixes, exactPrefix(addr))
			continue
		}
		if prefix, err := netip.ParsePrefix(e); err == nil {
			b.prefixes = append(b.prefixes, prefix)
			continue
		}
		return nil, fmt.Errorf("ip_blacklist entry %q is neither a valid IP nor a CIDR range", e)
	}
	return b, nil
}

// exactPrefix wraps a single IP as an exact-match prefix (/32 for IPv4, /128
// for IPv6).
func exactPrefix(addr netip.Addr) netip.Prefix {
	bits := 128
	if addr.Is4() {
		bits = 32
	}
	return netip.PrefixFrom(addr, bits)
}

// matches reports whether ip falls within any of the blacklisted prefixes.
func (b *ipBlacklist) matches(ip netip.Addr) bool {
	for _, p := range b.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// isEmpty reports whether no entries were configured (pass-through mode).
func (b *ipBlacklist) isEmpty() bool {
	return len(b.prefixes) == 0
}

// ipBlacklistMiddleware severs the connection for a request whose real IP —
// taken from the proxy headers via RealIP, since the physical peer is the
// upstream proxy — matches the blacklist. The connection is hijacked and
// closed, so no HTTP status line or response headers are ever sent to the
// client.
func ipBlacklistMiddleware(b *ipBlacklist) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if b.isEmpty() { // unconfigured: zero-cost pass-through
				return next(c)
			}
			addr, err := netip.ParseAddr(c.RealIP())
			if err != nil {
				// The header was empty or malformed, so the client IP cannot be
				// matched against the blacklist; let the request proceed rather
				// than dropping legitimate traffic on a parse failure.
				return next(c)
			}
			if b.matches(addr) {
				log.Printf("IP blacklist: dropping connection from %s", addr)
				if conn, _, hijackErr := c.Response().Hijack(); hijackErr == nil {
					conn.Close()
				}
				return nil // stop the chain; returning nil avoids a default response
			}
			return next(c)
		}
	}
}
