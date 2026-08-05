package traefik

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractHosts(t *testing.T) {
	tests := []struct {
		name string
		rule string
		want []string
	}{
		{
			name: "single host",
			rule: "Host(`app.internal.example.com`)",
			want: []string{"app.internal.example.com"},
		},
		{
			name: "multiple hosts in one matcher",
			rule: "Host(`app.internal.example.com`, `web.internal.example.com`)",
			want: []string{"app.internal.example.com", "web.internal.example.com"},
		},
		{
			name: "hosts combined with or",
			rule: "Host(`a.example.com`) || Host(`b.example.com`)",
			want: []string{"a.example.com", "b.example.com"},
		},
		{
			name: "host combined with path prefix",
			rule: "Host(`a.example.com`) && PathPrefix(`/api`)",
			want: []string{"a.example.com"},
		},
		{
			name: "grouped rule with mixed matchers",
			rule: "(Host(`a.example.com`) || Host(`b.example.com`)) && !PathPrefix(`/internal`)",
			want: []string{"a.example.com", "b.example.com"},
		},
		{
			name: "double quoted form",
			rule: `Host("a.example.com")`,
			want: []string{"a.example.com"},
		},
		{
			name: "single quoted form",
			rule: `Host('a.example.com')`,
			want: []string{"a.example.com"},
		},
		{
			name: "no whitespace around operators",
			rule: "Host(`a.example.com`)&&PathPrefix(`/x`)||Host(`b.example.com`)",
			want: []string{"a.example.com", "b.example.com"},
		},
		{
			name: "whitespace inside the matcher",
			rule: "Host( `a.example.com` , `b.example.com` )",
			want: []string{"a.example.com", "b.example.com"},
		},
		{
			name: "newlines in rule",
			rule: "Host(`a.example.com`)\n  && PathPrefix(`/api`)",
			want: []string{"a.example.com"},
		},
		{
			name: "HostRegexp is not a Host",
			rule: "HostRegexp(`^.+\\.example\\.com$`)",
			want: nil,
		},
		{
			name: "HostSNI is not a Host",
			rule: "HostSNI(`a.example.com`)",
			want: nil,
		},
		{
			name: "HostHeader-like matcher is not a Host",
			rule: "HostRegexp(`{subdomain:[a-z]+}.example.com`) || Host(`b.example.com`)",
			want: []string{"b.example.com"},
		},
		{
			name: "negated host is excluded",
			rule: "!Host(`blocked.example.com`)",
			want: nil,
		},
		{
			name: "negated host alongside a positive one",
			rule: "Host(`a.example.com`) && !Host(`blocked.example.com`)",
			want: []string{"a.example.com"},
		},
		{
			name: "negation with whitespace",
			rule: "! Host(`blocked.example.com`)",
			want: nil,
		},
		{
			name: "host inside another matcher's argument is not extracted",
			rule: "Headers(`X-Forwarded-Host`, `Host(`)",
			want: nil,
		},
		{
			name: "header value that looks like a hostname is not extracted",
			rule: "HeaderRegexp(`X-Real-Host`, `a.example.com`) && Host(`b.example.com`)",
			want: []string{"b.example.com"},
		},
		{
			name: "clienthost matcher is not a Host",
			rule: "ClientIP(`10.0.0.0/8`) && Host(`a.example.com`)",
			want: []string{"a.example.com"},
		},
		{
			name: "method and query matchers ignored",
			rule: "Method(`GET`) && Query(`ref`, `x`) && Host(`a.example.com`)",
			want: []string{"a.example.com"},
		},
		{
			name: "empty rule",
			rule: "",
			want: nil,
		},
		{
			name: "rule with no host matcher",
			rule: "PathPrefix(`/metrics`)",
			want: nil,
		},
		{
			name: "empty host matcher",
			rule: "Host()",
			want: nil,
		},
		{
			name: "unterminated quote does not panic",
			rule: "Host(`a.example.com",
			want: []string{"a.example.com"},
		},
		{
			name: "unterminated parenthesis does not panic",
			rule: "Host(`a.example.com`",
			want: []string{"a.example.com"},
		},
		{
			name: "stray backtick does not panic",
			rule: "Host(`a.example.com`) && `",
			want: []string{"a.example.com"},
		},
		{
			name: "bare unquoted argument is skipped safely",
			rule: "Host(a.example.com) && Host(`b.example.com`)",
			want: []string{"b.example.com"},
		},
		{
			name: "trailing operator does not panic",
			rule: "Host(`a.example.com`) &&",
			want: []string{"a.example.com"},
		},
		{
			name: "lone bang does not panic",
			rule: "!",
			want: nil,
		},
		{
			name: "repeated host is returned twice before dedup",
			rule: "Host(`a.example.com`) || Host(`a.example.com`)",
			want: []string{"a.example.com", "a.example.com"},
		},
		{
			name: "case is preserved at extraction",
			rule: "Host(`A.Example.COM`)",
			want: []string{"A.Example.COM"},
		},
		{
			name: "trailing dot preserved at extraction",
			rule: "Host(`a.example.com.`)",
			want: []string{"a.example.com."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractHosts(tc.rule)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractHosts(%q)\n got: %#v\nwant: %#v", tc.rule, got, tc.want)
			}
		})
	}
}

func TestHostsInZones(t *testing.T) {
	zones := []string{"internal.example.com."}

	tests := []struct {
		name  string
		rule  string
		zones []string
		want  []string
	}{
		{
			name: "in zone",
			rule: "Host(`app.internal.example.com`)",
			want: []string{"app.internal.example.com."},
		},
		{
			name: "out of zone is dropped",
			rule: "Host(`public.example.com`)",
			want: nil,
		},
		{
			name: "sibling zone with a shared suffix string is dropped",
			rule: "Host(`notinternal.example.com`)",
			want: nil,
		},
		{
			name: "mixed in and out of zone",
			rule: "Host(`app.internal.example.com`, `www.example.com`)",
			want: []string{"app.internal.example.com."},
		},
		{
			name: "deep subdomain is in zone",
			rule: "Host(`a.b.internal.example.com`)",
			want: []string{"a.b.internal.example.com."},
		},
		{
			name: "zone apex is in zone",
			rule: "Host(`internal.example.com`)",
			want: []string{"internal.example.com."},
		},
		{
			name: "case is normalized",
			rule: "Host(`App.Internal.Example.Com`)",
			want: []string{"app.internal.example.com."},
		},
		{
			name: "trailing dot is normalized",
			rule: "Host(`app.internal.example.com.`)",
			want: []string{"app.internal.example.com."},
		},
		{
			name: "duplicates are collapsed",
			rule: "Host(`a.internal.example.com`) || Host(`A.internal.example.com.`)",
			want: []string{"a.internal.example.com."},
		},
		{
			name: "wildcard is not a host",
			rule: "Host(`*.internal.example.com`)",
			want: nil,
		},
		{
			name: "regexp placeholder is not a host",
			rule: "Host(`{sub:[a-z]+}.internal.example.com`)",
			want: nil,
		},
		{
			name: "empty host is dropped",
			rule: "Host(``)",
			want: nil,
		},
		{
			name:  "multiple zones served",
			rule:  "Host(`a.internal.example.com`) || Host(`b.lab.example.com`)",
			zones: []string{"internal.example.com.", "lab.example.com."},
			want:  []string{"a.internal.example.com.", "b.lab.example.com."},
		},
		{
			name: "label longer than 63 bytes is rejected",
			rule: "Host(`" + strings.Repeat("a", 64) + ".internal.example.com`)",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := tc.zones
			if z == nil {
				z = zones
			}
			got := hostsInZones(tc.rule, z)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hostsInZones(%q, %v)\n got: %#v\nwant: %#v", tc.rule, z, got, tc.want)
			}
		})
	}
}

// Real router rules as they come back from /api/http/routers, to keep the
// scanner honest about the shapes Traefik actually emits.
func TestHostsInZonesRealWorldRules(t *testing.T) {
	zones := []string{"internal.example.com."}

	tests := []struct {
		rule string
		want []string
	}{
		{
			rule: "Host(`app.internal.example.com`)",
			want: []string{"app.internal.example.com."},
		},
		{
			rule: "Host(`traefik.internal.example.com`) && (PathPrefix(`/api`) || PathPrefix(`/dashboard`))",
			want: []string{"traefik.internal.example.com."},
		},
		{
			rule: "Host(`wiki.internal.example.com`) || Host(`docs.internal.example.com`)",
			want: []string{"wiki.internal.example.com.", "docs.internal.example.com."},
		},
		{
			rule: "Host(`auth.internal.example.com`) && PathPrefix(`/api/verify`)",
			want: []string{"auth.internal.example.com."},
		},
		{
			rule: "(Host(`a.internal.example.com`) || Host(`b.internal.example.com`)) && Method(`GET`)",
			want: []string{"a.internal.example.com.", "b.internal.example.com."},
		},
		{
			// Traefik's own internal API router.
			rule: "PathPrefix(`/api`)",
			want: nil,
		},
		{
			// A public router that must not leak into the internal zone.
			rule: "Host(`www.example.com`) && !PathPrefix(`/admin`)",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.rule, func(t *testing.T) {
			got := hostsInZones(tc.rule, zones)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hostsInZones(%q)\n got: %#v\nwant: %#v", tc.rule, got, tc.want)
			}
		})
	}
}
