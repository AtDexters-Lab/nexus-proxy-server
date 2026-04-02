package hub

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Additional non-routable or internal ranges not covered by net.IP.IsPrivate.
var (
	// CGNAT / Shared Address Space (RFC 6598) — used by cloud VPCs for
	// internal services, NAT gateways, and service mesh endpoints.
	cgnatBlock = net.IPNet{IP: net.IP{100, 64, 0, 0}, Mask: net.CIDRMask(10, 32)}
	// Benchmarking (RFC 2544) — reserved for network testing.
	benchBlock = net.IPNet{IP: net.IP{198, 18, 0, 0}, Mask: net.CIDRMask(15, 32)}
)

// rejectPrivateIP returns an error if the IP is a private, loopback,
// link-local, unspecified, CGNAT, benchmarking, or cloud metadata address.
func rejectPrivateIP(ip net.IP) error {
	// Unmap IPv6-mapped IPv4 (e.g., ::ffff:127.0.0.1 → 127.0.0.1)
	if mapped := ip.To4(); mapped != nil {
		ip = mapped
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("IP %s is not allowed (private/loopback/link-local)", ip)
	}
	if cgnatBlock.Contains(ip) || benchBlock.Contains(ip) {
		return fmt.Errorf("IP %s is not allowed (CGNAT/benchmarking range)", ip)
	}
	// Block cloud metadata endpoint
	if ip.Equal(net.IPv4(169, 254, 169, 254)) {
		return fmt.Errorf("IP %s is a metadata endpoint", ip)
	}
	return nil
}

// ssrfSafeDialContext resolves the host, validates all IPs against
// rejectPrivateIP, then tries each validated IP in order until one connects
// (matching the standard library's fallback behavior).
func ssrfSafeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses resolved for %s", host)
	}
	for _, ip := range ips {
		if err := rejectPrivateIP(ip.IP); err != nil {
			return nil, fmt.Errorf("resolved IP blocked: %w", err)
		}
	}
	// Try each validated address in order, falling back on dial failure.
	var lastErr error
	var d net.Dialer
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// newSSRFSafeClient returns an HTTP client that validates resolved IPs,
// disables redirects, and preserves the provided TLS and proxy config.
func newSSRFSafeClient(baseClient *http.Client) *http.Client {
	var tlsCfg *tls.Config
	if baseClient != nil {
		if t, ok := baseClient.Transport.(*http.Transport); ok && t.TLSClientConfig != nil {
			tlsCfg = t.TLSClientConfig.Clone()
		}
	}
	// No proxy: ssrfSafeDialContext validates the dialed IP, which would be
	// the proxy address (not the target) when a proxy is configured, defeating
	// the SSRF protection. Health checks go direct.
	transport := &http.Transport{
		DialContext:       ssrfSafeDialContext,
		TLSClientConfig:  tlsCfg,
		DisableKeepAlives: true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
