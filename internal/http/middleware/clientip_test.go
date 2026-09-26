package middleware

import (
	"net/netip"
	"testing"
)

func TestClientIPResolver(t *testing.T) {
	platform := mustPrefixes("10.0.0.0/8") // e.g. the hosting platform's edge proxies
	const cfEdge = "172.70.1.2"            // inside 172.64.0.0/13
	const client = "196.201.214.10"

	tests := []struct {
		name       string
		r          ClientIPResolver
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{"direct, no trust", ClientIPResolver{}, client + ":5555", nil, client},
		{
			"spoofed headers ignored without trust",
			ClientIPResolver{},
			client + ":5555",
			map[string]string{"X-Forwarded-For": "1.2.3.4", "CF-Connecting-IP": "1.2.3.4"},
			client,
		},
		{
			"CF header trusted from a Cloudflare edge",
			ClientIPResolver{TrustCloudflare: true},
			cfEdge + ":443",
			map[string]string{"CF-Connecting-IP": client},
			client,
		},
		{
			"CF header ignored from a non-Cloudflare peer (spoof)",
			ClientIPResolver{TrustCloudflare: true},
			"203.0.113.9:443",
			map[string]string{"CF-Connecting-IP": "1.2.3.4"},
			"203.0.113.9",
		},
		{
			"platform proxy -> Cloudflare -> client",
			ClientIPResolver{TrustedProxies: platform, TrustCloudflare: true},
			"10.1.2.3:8080",
			map[string]string{"X-Forwarded-For": cfEdge, "CF-Connecting-IP": client},
			client,
		},
		{
			"platform proxy, spoofed XFF prefix ignored",
			ClientIPResolver{TrustedProxies: platform, TrustCloudflare: true},
			"10.1.2.3:8080",
			map[string]string{"X-Forwarded-For": "1.2.3.4, 203.0.113.9", "CF-Connecting-IP": "1.2.3.4"},
			"203.0.113.9",
		},
		{
			"platform proxy without Cloudflare",
			ClientIPResolver{TrustedProxies: platform},
			"10.1.2.3:8080",
			map[string]string{"X-Forwarded-For": "9.9.9.9, " + client},
			client,
		},
		{
			"garbage CF header falls back to edge",
			ClientIPResolver{TrustCloudflare: true},
			cfEdge + ":443",
			map[string]string{"CF-Connecting-IP": "not-an-ip"},
			cfEdge,
		},
		{
			"IPv6 Cloudflare edge and client",
			ClientIPResolver{TrustCloudflare: true},
			"[2606:4700::1]:443",
			map[string]string{"CF-Connecting-IP": "2c0f:f248:1:2::5"},
			"2c0f:f248:1:2::5",
		},
		{"IPv4-mapped peer", ClientIPResolver{}, "[::ffff:196.201.214.10]:1", nil, client},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.r.Resolve(tt.remoteAddr, func(k string) string { return tt.headers[k] })
			if got != netip.MustParseAddr(tt.want) {
				t.Errorf("Resolve = %v, want %s", got, tt.want)
			}
		})
	}
}
