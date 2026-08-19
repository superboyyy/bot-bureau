package config

import (
	"botbureau/backend/internal/i18n"

	"fmt"
	"net"
	"net/url"
	"strings"
)

// NormalizeFetchHosts turns pasted URLs and host:port pairs into lowercase hostnames, drops blanks,
// and dedupes. The empty list is the public-internet policy: fetch_url may open any public host.
func NormalizeFetchHosts(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		h := normalizeFetchHost(s)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

func normalizeFetchHost(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	}
	s = strings.TrimSuffix(s, ".")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return strings.ToLower(strings.Trim(host, "[]"))
	}
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return strings.ToLower(s)
}

// HostAllowed reports whether host may be fetched. An empty list allows every host (the dialer still
// refuses loopback and private addresses). A non-empty list is exact hostname match, case-insensitive.
func HostAllowed(host string, list []string) bool {
	if len(list) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, h := range list {
		if host == h {
			return true
		}
	}
	return false
}

func HostAllowedErr(host string, list []string) error {
	if HostAllowed(host, list) {
		return nil
	}
	return fmt.Errorf(i18n.T("%s is not on the fetch allowlist"), host)
}
