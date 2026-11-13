package webtools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// BlockedAddressError reports a destination that web_fetch refuses because it
// is not on the public internet.
type BlockedAddressError struct {
	Host string
	IP   netip.Addr
	Why  string
}

func (e *BlockedAddressError) Error() string {
	if e.Host != "" && e.Host != e.IP.String() {
		return fmt.Sprintf("blocked: %s resolves to %s, a %s address; web_fetch only reaches public internet hosts", e.Host, e.IP, e.Why)
	}
	return fmt.Sprintf("blocked: %s is a %s address; web_fetch only reaches public internet hosts", e.IP, e.Why)
}

// blockedPrefixes are the ranges web_fetch never connects to, with the
// reason shown to the model. IPv4-mapped IPv6 addresses are unmapped first.
var blockedPrefixes = []struct {
	prefix netip.Prefix
	why    string
}{
	{netip.MustParsePrefix("0.0.0.0/8"), "\"this network\""},
	{netip.MustParsePrefix("10.0.0.0/8"), "private"},
	{netip.MustParsePrefix("100.64.0.0/10"), "carrier-grade NAT"},
	{netip.MustParsePrefix("127.0.0.0/8"), "loopback"},
	{netip.MustParsePrefix("169.254.0.0/16"), "link-local (cloud metadata)"},
	{netip.MustParsePrefix("172.16.0.0/12"), "private"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignment"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation"},
	{netip.MustParsePrefix("192.88.99.0/24"), "6to4 relay"},
	{netip.MustParsePrefix("192.168.0.0/16"), "private"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation"},
	{netip.MustParsePrefix("224.0.0.0/4"), "multicast"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},
	{netip.MustParsePrefix("::/128"), "unspecified"},
	{netip.MustParsePrefix("::1/128"), "loopback"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "local NAT64"},
	{netip.MustParsePrefix("100::/64"), "discard"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation"},
	{netip.MustParsePrefix("fc00::/7"), "unique local (private)"},
	{netip.MustParsePrefix("fe80::/10"), "link-local"},
	{netip.MustParsePrefix("fec0::/10"), "site-local"},
	{netip.MustParsePrefix("ff00::/8"), "multicast"},
}

// embedded IPv6 ranges that carry an IPv4 address the check must look at.
var (
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour   = netip.MustParsePrefix("2002::/16")
)

// blockReason says why ip is not a public destination, or "" when it is.
func blockReason(ip netip.Addr) string {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() {
		return "invalid"
	}
	for _, b := range blockedPrefixes {
		if b.prefix.Contains(ip) {
			return b.why
		}
	}
	// NAT64 and 6to4 addresses embed an IPv4 address; judge that one.
	if nat64Prefix.Contains(ip) {
		a := ip.As16()
		return blockReason(netip.AddrFrom4([4]byte{a[12], a[13], a[14], a[15]}))
	}
	if sixToFour.Contains(ip) {
		a := ip.As16()
		return blockReason(netip.AddrFrom4([4]byte{a[2], a[3], a[4], a[5]}))
	}
	return ""
}

// guard decides which addresses may be dialled.
type guard struct {
	// allowLoopback lets tests reach httptest servers. It is never set
	// outside tests.
	allowLoopback bool
	lookup        func(ctx context.Context, host string) ([]netip.Addr, error)
}

func (g *guard) check(host string, ip netip.Addr) error {
	why := blockReason(ip)
	if why == "" {
		return nil
	}
	if g.allowLoopback && ip.Unmap().IsLoopback() {
		return nil
	}
	return &BlockedAddressError{Host: host, IP: ip.Unmap(), Why: why}
}

// resolve returns the addresses of host that may be dialled. Any blocked
// address fails the whole lookup, so a name that mixes public and private
// records cannot be used to reach the private one.
func (g *guard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" {
		return nil, errors.New("empty host")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if err := g.check(host, ip); err != nil {
			return nil, err
		}
		return []netip.Addr{ip.Unmap()}, nil
	}
	if isLocalName(host) {
		return nil, fmt.Errorf("blocked: %s is a local host name; web_fetch only reaches public internet hosts", host)
	}
	ips, err := g.lookup(ctx, host)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, fmt.Errorf("could not resolve host %s", host)
		}
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("could not resolve host %s", host)
	}
	for _, ip := range ips {
		if err := g.check(host, ip); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

// isLocalName catches names that resolve locally without DNS.
func isLocalName(host string) bool {
	h := strings.ToLower(host)
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") ||
		strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".home.arpa")
}

// dialContext resolves the host itself, checks every address and connects
// to a vetted IP, so a DNS answer cannot change between check and connect.
func (g *guard) dialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := g.resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		var firstErr error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				break
			}
		}
		return nil, firstErr
	}
}

func systemLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}
