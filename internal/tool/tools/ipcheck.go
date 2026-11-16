package tools

import "net/netip"

// cgnat is 100.64.0.0/10 (RFC 6598), which netip has no predicate for.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// classifyIP returns a short reason why an address must not be dialled, or ""
// when it is a fine public destination. IPv4-mapped IPv6 forms
// (::ffff:10.0.0.1) are unmapped first, so they cannot smuggle a blocked
// range past the checks.
func classifyIP(ip netip.Addr) string {
	if !ip.IsValid() {
		return "invalid address"
	}
	ip = ip.Unmap()
	switch {
	case ip.IsUnspecified():
		return "unspecified address"
	case ip.IsLoopback():
		return "loopback address"
	case ip.IsLinkLocalUnicast():
		return "link-local address"
	case ip.IsMulticast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return "multicast address"
	case ip.IsPrivate():
		if ip.Is6() {
			return "unique-local address"
		}
		return "private address"
	case ip.Is4() && cgnat.Contains(ip):
		return "carrier-grade NAT address"
	}
	// Reserved and transition ranges: they are either not routable or a relay
	// that can reach an embedded IPv4 address the checks above never saw.
	for _, r := range reserved {
		if r.net.Contains(ip) {
			return r.why
		}
	}
	return ""
}

var reserved = []struct {
	net netip.Prefix
	why string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), "reserved address"},
	{netip.MustParsePrefix("192.0.0.0/24"), "reserved address"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation address"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking address"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation address"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation address"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved address"},
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64 address"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "NAT64 address"},
	{netip.MustParsePrefix("2002::/16"), "6to4 address"},
	{netip.MustParsePrefix("2001::/32"), "Teredo address"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation address"},
	{netip.MustParsePrefix("::/96"), "IPv4-compatible address"},
}
