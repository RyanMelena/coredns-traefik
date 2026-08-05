package traefik

import (
	"strings"

	"github.com/coredns/coredns/plugin"
	"github.com/miekg/dns"
)

// hostMatcher is the only Traefik matcher we can enumerate names from.
//
// HostRegexp is deliberately ignored: a regexp cannot be expanded into a finite
// set of names, and guessing would reintroduce the wildcard behaviour this
// plugin exists to avoid. HostSNI only appears on TCP routers, which are not
// served by the endpoint we poll.
const hostMatcher = "Host"

// extractHosts returns every hostname appearing in a positive Host(...) matcher
// within a Traefik router rule, in order of appearance and without
// normalization.
//
// The rule is scanned rather than pattern-matched. A regexp over `Host(...)`
// would also match HostRegexp and HostSNI, and would happily pull "hostnames"
// out of unrelated quoted arguments such as Headers(`X-Forwarded-Host`, `...`).
// Scanning also means a malformed rule simply truncates the walk instead of
// producing garbage or panicking.
func extractHosts(rule string) []string {
	var hosts []string

	for i := 0; i < len(rule); {
		c := rule[i]

		switch {
		case isQuote(c):
			// A quoted string reached outside of a Host argument list belongs to
			// some other matcher. Skip it whole so its contents can never be
			// scanned as an identifier or a host.
			_, i = readQuoted(rule, i)

		case isIdentByte(c):
			name, next := readIdent(rule, i)
			open := skipSpace(rule, next)
			if open == len(rule) || rule[open] != '(' || name != hostMatcher || negated(rule, i) {
				// Not a call, not Host, or a negated Host (which excludes names
				// rather than defining them). Advance past the identifier only:
				// the argument list is then scanned normally, so nested calls
				// and quoted arguments are still handled.
				i = next
				continue
			}
			var args []string
			args, i = readArgs(rule, open)
			hosts = append(hosts, args...)

		default:
			i++
		}
	}

	return hosts
}

// hostsInZones extracts the hosts from rule that fall inside one of zones,
// normalized to lowercase FQDNs with a trailing dot. Results are deduplicated
// and keep their order of appearance. zones must already be normalized, as
// plugin.OriginsFromArgsOrServerBlock returns them.
func hostsInZones(rule string, zones []string) []string {
	var (
		kept []string
		seen map[string]struct{}
	)

	for _, h := range extractHosts(rule) {
		name, ok := normalizeHost(h)
		if !ok {
			continue
		}
		if plugin.Zones(zones).Matches(name) == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		if seen == nil {
			seen = make(map[string]struct{})
		}
		seen[name] = struct{}{}
		kept = append(kept, name)
	}

	return kept
}

// normalizeHost turns a host as written in a rule into a lowercase FQDN, or
// reports false if it is not a plain hostname we can serve an A record for.
func normalizeHost(h string) (string, bool) {
	h = strings.TrimSpace(h)
	if h == "" || h == "." {
		return "", false
	}

	// Traefik v1 style placeholders and regexp fragments occasionally survive in
	// hand-written rules. They name a pattern, not a host.
	if strings.ContainsAny(h, "{}*?[]()\\/ \t") {
		return "", false
	}

	h = dns.CanonicalName(h)
	if _, ok := dns.IsDomainName(h); !ok {
		return "", false
	}

	return h, true
}

// readIdent consumes the identifier starting at i and returns it with the index
// of the first byte after it.
func readIdent(s string, i int) (string, int) {
	start := i
	for i < len(s) && isIdentByte(s[i]) {
		i++
	}
	return s[start:i], i
}

// readQuoted consumes the quoted string starting at i (s[i] must be a quote) and
// returns its contents with the index of the first byte after the closing quote.
// An unterminated string consumes the remainder of s.
func readQuoted(s string, i int) (string, int) {
	quote := s[i]
	i++
	var b strings.Builder
	for i < len(s) {
		switch {
		// Backticks are raw in Traefik rules, exactly as in Go, so a backslash
		// inside them is a literal backslash.
		case s[i] == '\\' && quote != '`' && i+1 < len(s):
			b.WriteByte(s[i+1])
			i += 2
		case s[i] == quote:
			return b.String(), i + 1
		default:
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String(), i
}

// readArgs consumes the argument list starting at i (s[i] must be '(') and
// returns the contents of its quoted arguments. Anything unexpected - a bare
// word, a nested call, a truncated rule - ends the list and hands the position
// back to the caller's scan rather than being guessed at.
func readArgs(s string, i int) ([]string, int) {
	var args []string
	i++ // past '('
	for i < len(s) {
		i = skipSpace(s, i)
		if i == len(s) {
			break
		}
		switch {
		case s[i] == ')':
			return args, i + 1
		case s[i] == ',':
			i++
		case isQuote(s[i]):
			var arg string
			arg, i = readQuoted(s, i)
			args = append(args, arg)
		default:
			return args, i
		}
	}
	return args, i
}

// negated reports whether the identifier starting at i is preceded by a '!'.
func negated(s string, i int) bool {
	for i--; i >= 0; i-- {
		if isSpace(s[i]) {
			continue
		}
		return s[i] == '!'
	}
	return false
}

func skipSpace(s string, i int) int {
	for i < len(s) && isSpace(s[i]) {
		i++
	}
	return i
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func isQuote(c byte) bool { return c == '`' || c == '"' || c == '\'' }

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
