package middleware

import (
	"fmt"
	"strconv"
	"strings"
)

// OriginMatcher decides whether a browser Origin is allowed. It is the one
// implementation of ALLOWED_ORIGINS, used by both the HTTP CORS middleware
// and the WebSocket upgrade's origin check, so the two cannot disagree.
//
// Each entry of the list is one of:
//
//   - "*": any origin. It must be the only entry.
//   - "https://gamemesh.app": exactly this scheme, host and (absent) port.
//   - "https://*.gamemesh.app": any subdomain of gamemesh.app, at any depth,
//     over https; not gamemesh.app itself, and not evilgamemesh.app or
//     gamemesh.app.evil.example.
//   - "http://localhost:*": this host on any port, or with no port.
//
// Scheme and host compare case-insensitively. An origin with a path, query,
// fragment or user info (which a browser never sends) matches nothing, and
// neither does the opaque origin "null" unless the list is "*".
type OriginMatcher struct {
	any   bool
	rules []originRule
}

type originRule struct {
	scheme string
	host   string // lower case; for a subdomain rule, the parent domain
	sub    bool   // host is "*." + host
	port   string // "" for none, "*" for any (or none), else the digits
}

// ParseOrigins builds a matcher from ALLOWED_ORIGINS entries. An entry it
// cannot parse is an error, never a rule that silently matches nothing, so a
// service can refuse to start on a typo.
func ParseOrigins(entries []string) (*OriginMatcher, error) {
	m := &OriginMatcher{}
	for _, e := range entries {
		if e == "*" {
			m.any = true
			continue
		}
		r, err := parseOriginRule(e)
		if err != nil {
			return nil, fmt.Errorf("allowed origin %q: %w", e, err)
		}
		m.rules = append(m.rules, r)
	}
	if m.any && len(entries) > 1 {
		return nil, fmt.Errorf("allowed origins: \"*\" must be the only entry, got %q", entries)
	}
	return m, nil
}

// AllowsAny reports whether the list is "*". A nil matcher allows nothing.
func (m *OriginMatcher) AllowsAny() bool { return m != nil && m.any }

// Allowed reports whether origin, the value of an Origin header, matches the
// list. The empty string (no Origin header) is not an origin and matches
// nothing here; callers that accept such requests say so themselves.
func (m *OriginMatcher) Allowed(origin string) bool {
	if m == nil {
		return false
	}
	if m.any {
		return true
	}
	scheme, host, port, ok := splitOrigin(origin)
	if !ok || !validHost(host) || (port != "" && !validPort(port)) {
		return false
	}
	for _, r := range m.rules {
		if r.scheme != scheme {
			continue
		}
		if r.port != "*" && r.port != port {
			continue
		}
		if r.sub {
			if len(host) > len(r.host)+1 && strings.HasSuffix(host, "."+r.host) {
				return true
			}
			continue
		}
		if host == r.host {
			return true
		}
	}
	return false
}

func parseOriginRule(e string) (originRule, error) {
	scheme, host, port, ok := splitOrigin(e)
	if !ok {
		return originRule{}, fmt.Errorf("want scheme://host[:port], with no path or trailing slash")
	}
	r := originRule{scheme: scheme, host: host, port: port}
	if rest, found := strings.CutPrefix(host, "*."); found {
		r.sub, r.host = true, rest
	}
	if !validHost(r.host) {
		return originRule{}, fmt.Errorf("invalid host %q (a wildcard must be the whole leftmost label, as in *.example.com)", host)
	}
	if port != "" && port != "*" && !validPort(port) {
		return originRule{}, fmt.Errorf("invalid port %q", port)
	}
	return r, nil
}

// splitOrigin splits scheme://host[:port] into a lower-case scheme and host
// and the port. It fails on anything else, including a path, query,
// fragment, user info or a scheme other than http and https.
func splitOrigin(s string) (scheme, host, port string, ok bool) {
	scheme, hostport, found := strings.Cut(s, "://")
	if !found {
		return "", "", "", false
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", "", false
	}
	if hostport == "" || strings.ContainsAny(hostport, "/?#@\\ ") {
		return "", "", "", false
	}
	host = hostport
	if strings.HasPrefix(hostport, "[") { // IPv6 literal
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return "", "", "", false
		}
		host = hostport[:end+1]
		switch rest := hostport[end+1:]; {
		case rest == "":
		case rest[0] == ':' && len(rest) > 1:
			port = rest[1:]
		default:
			return "", "", "", false
		}
	} else if i := strings.LastIndexByte(hostport, ':'); i >= 0 {
		host, port = hostport[:i], hostport[i+1:]
		if port == "" {
			return "", "", "", false
		}
	}
	return scheme, strings.ToLower(host), port, true
}

// validHost accepts a DNS name or IPv4 address (dot-separated labels of
// letters, digits and hyphens) or a bracketed IPv6 literal.
func validHost(h string) bool {
	if strings.HasPrefix(h, "[") {
		inner := strings.TrimSuffix(h[1:], "]")
		return len(inner) == len(h)-2 && inner != "" &&
			strings.Trim(inner, "0123456789abcdef:.") == ""
	}
	if h == "" {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == p
}
