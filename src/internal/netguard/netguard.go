// Package netguard checks a caller-supplied media URL before yt-dlp sees it.
//
// Only absolute http(s) URLs without credentials, whose host resolves to
// public addresses (unless the administrator enabled private addresses for
// LAN tests).
//
// Residual risk: yt-dlp follows redirects and extractors fetch further URLs
// (APIs, CDNs, manifests) that this check never sees, and DNS can change
// between the check and the connection. The generic extractor can be turned
// off (network.allow_generic_extractor) to limit fetching to known sites.
package netguard

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/openplayx/mcp-yt-dlp/src/internal/apierr"
)

const MaxURLLength = 2048

// Resolver returns the addresses of host.
type Resolver func(ctx context.Context, host string, port int) ([]string, error)

func SystemResolver(ctx context.Context, host string, port int) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// Ranges that are not globally reachable although netip calls them global
// unicast (mirrors Python's ipaddress is_global).
var special = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32",
		"64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// AddressOK reports whether addr is a public unicast address.
func AddressOK(addr string) bool {
	a, err := netip.ParseAddr(strings.SplitN(addr, "%", 2)[0])
	if err != nil {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range special {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// CheckURL returns the trimmed URL or a url_blocked error.
func CheckURL(ctx context.Context, raw string, allowPrivate bool, resolve Resolver) (string, *apierr.Error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > MaxURLLength || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return "", apierr.URLBlocked("The URL is empty, too long or contains spaces.")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", apierr.URLBlocked("The URL is malformed.")
	}
	port := 0
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return "", apierr.URLBlocked("The URL is malformed.")
		}
		port = n
	}
	host := strings.ToLower(u.Hostname())
	if (u.Scheme != "http" && u.Scheme != "https") || host == "" {
		return "", apierr.URLBlocked("Only absolute http(s) URLs are accepted.")
	}
	if u.User != nil {
		return "", apierr.URLBlocked("URLs with credentials are not accepted.")
	}
	if allowPrivate {
		return raw, nil
	}
	if port == 0 {
		port = 80
		if u.Scheme == "https" {
			port = 443
		}
	}
	var addrs []string
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addrs = []string{strings.Trim(host, "[]")}
	} else {
		if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
			strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".lan") {
			return "", apierr.URLBlocked("Local addresses are not accepted.")
		}
		addrs, err = resolve(ctx, host, port)
		if err != nil {
			return "", apierr.URLBlocked("The URL host does not resolve.")
		}
	}
	if len(addrs) == 0 {
		return "", apierr.URLBlocked("Private or local addresses are not accepted.")
	}
	for _, a := range addrs {
		if !AddressOK(a) {
			return "", apierr.URLBlocked("Private or local addresses are not accepted.")
		}
	}
	return raw, nil
}
