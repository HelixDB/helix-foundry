package connector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// ErrPrivateAddress is a customer host that resolves to an address connectors
// must not reach from inside our network.
var ErrPrivateAddress = errors.New("connector: host resolves to a non-public address")

// blockedPrefixes are public-looking ranges that still reach infrastructure:
// carrier-grade NAT (which also holds Alibaba's metadata endpoint), benchmarking,
// IETF protocol assignments, and the IPv6 discard and documentation ranges.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// Guard decides which addresses connectors may reach. Customer hosts are dialed
// from inside our VPC, so loopback, private, link-local (cloud metadata) and
// other non-public addresses are refused unless AllowPrivate, which is for
// local development only. It dials the address it vetted, so a second DNS
// answer cannot redirect the connection.
type Guard struct {
	AllowPrivate bool
	Resolver     *net.Resolver
}

func (g Guard) permits(addr netip.Addr) bool {
	addr = addr.Unmap()
	if g.AllowPrivate {
		return addr.IsValid()
	}
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// Resolve returns the host's addresses, refusing the host if any is not permitted.
// One bad answer refuses the host, so a name cannot mix a public and a private record.
func (g Guard) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if !g.permits(addr) {
			return nil, ErrPrivateAddress
		}
		return []netip.Addr{addr}, nil
	}
	resolver := g.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	addrs, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, addr := range addrs {
		if !g.permits(addr) {
			return nil, ErrPrivateAddress
		}
	}
	return addrs, nil
}

// DialContext dials host:port on the first vetted address that answers.
func (g Guard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := g.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var lastErr error
	for _, addr := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// Transport dials every connection through the guard and never uses a proxy.
func (g Guard) Transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = g.DialContext
	return transport
}

// HTTPClient is a client whose every connection goes through the guard and
// which never follows redirects, so a source cannot bounce a request inward.
func (g Guard) HTTPClient() *http.Client {
	return &http.Client{
		Transport:     g.Transport(),
		Timeout:       30 * time.Second,
		CheckRedirect: noRedirects,
	}
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
