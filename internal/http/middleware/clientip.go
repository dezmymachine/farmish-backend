package middleware

import (
	"net"
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
)

const clientIPKey = "client_ip"

// cloudflareRanges are Cloudflare's published edge ranges
// (https://www.cloudflare.com/ips/, fetched 2026-09-26). Refresh when
// Cloudflare announces changes; a missing range only means those edges'
// CF-Connecting-IP is ignored (we fall back to the edge address), never that
// a spoofed header is trusted.
var cloudflareRanges = mustPrefixes(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
)

// ClientIPResolver works out the real client address.
//
//  1. Start from the TCP peer. While it is a trusted proxy (TRUSTED_PROXIES,
//     e.g. the hosting platform's edge), step back one hop through
//     X-Forwarded-For, right to left.
//  2. If TRUST_CLOUDFLARE is on and that hop is a Cloudflare edge, use
//     CF-Connecting-IP.
//
// Headers are never trusted from anyone else, so a client connecting directly
// can't spoof its address.
type ClientIPResolver struct {
	TrustedProxies  []netip.Prefix
	TrustCloudflare bool
}

// Resolve returns the client address for a request.
func (r ClientIPResolver) Resolve(remoteAddr string, header func(string) string) netip.Addr {
	peer := parseAddr(remoteAddr)
	if !peer.IsValid() {
		return peer
	}
	hops := forwardedHops(header("X-Forwarded-For"))
	for inAny(peer, r.TrustedProxies) && len(hops) > 0 {
		peer, hops = hops[len(hops)-1], hops[:len(hops)-1]
	}
	if r.TrustCloudflare && inAny(peer, cloudflareRanges) {
		if cf := parseAddr(header("CF-Connecting-IP")); cf.IsValid() {
			return cf
		}
	}
	return peer
}

// ClientIP stores the resolved client address on the Gin context for the
// access log, rate limiter and Turnstile.
func ClientIP(r ClientIPResolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(clientIPKey, r.Resolve(c.Request.RemoteAddr, c.GetHeader))
		c.Next()
	}
}

// GetClientIP returns the address set by ClientIP, falling back to the TCP
// peer when the middleware isn't installed.
func GetClientIP(c *gin.Context) netip.Addr {
	if v, ok := c.Get(clientIPKey); ok {
		if a, ok := v.(netip.Addr); ok {
			return a
		}
	}
	return parseAddr(c.Request.RemoteAddr)
}

func forwardedHops(xff string) []netip.Addr {
	var hops []netip.Addr
	for _, part := range strings.Split(xff, ",") {
		if a := parseAddr(strings.TrimSpace(part)); a.IsValid() {
			hops = append(hops, a)
		}
	}
	return hops
}

// parseAddr accepts "ip", "ip:port" and "[ipv6]:port".
func parseAddr(s string) netip.Addr {
	if s == "" {
		return netip.Addr{}
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

func inAny(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}
