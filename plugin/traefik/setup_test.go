package traefik

import (
	"strings"
	"testing"
	"time"

	"github.com/coredns/caddy"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		zones        []string
		wantErr      string // substring; empty means the config must be accepted
		wantAPI      string
		wantTarget   string
		wantInterval time.Duration
		wantTTL      uint32
		wantTimeout  time.Duration
	}{
		{
			name: "minimal config takes the defaults",
			input: `traefik {
				api    http://10.0.0.1:8080/api/http/routers
				target 10.0.0.2
			}`,
			wantAPI:      "http://10.0.0.1:8080/api/http/routers",
			wantTarget:   "10.0.0.2",
			wantInterval: defaultInterval,
			wantTTL:      defaultTTL,
			wantTimeout:  defaultTimeout,
		},
		{
			name: "every option set",
			input: `traefik {
				api      https://traefik.example.com/api/http/routers
				target   192.168.1.10
				interval 15s
				ttl      300
				timeout  2s
			}`,
			wantAPI:      "https://traefik.example.com/api/http/routers",
			wantTarget:   "192.168.1.10",
			wantInterval: 15 * time.Second,
			wantTTL:      300,
			wantTimeout:  2 * time.Second,
		},
		{
			name: "interval accepts minutes",
			input: `traefik {
				api      http://t/api/http/routers
				target   10.0.0.2
				interval 1m30s
			}`,
			wantAPI:      "http://t/api/http/routers",
			wantTarget:   "10.0.0.2",
			wantInterval: 90 * time.Second,
			wantTTL:      defaultTTL,
			wantTimeout:  defaultTimeout,
		},
		{
			name: "ttl of zero is allowed",
			input: `traefik {
				api    http://t/api/http/routers
				target 10.0.0.2
				ttl    0
			}`,
			wantAPI:      "http://t/api/http/routers",
			wantTarget:   "10.0.0.2",
			wantInterval: defaultInterval,
			wantTTL:      0,
			wantTimeout:  defaultTimeout,
		},

		// Missing required values. An unset environment variable substitutes to
		// nothing, so these are the shapes a missing env var actually takes.
		{
			name: "missing api",
			input: `traefik {
				target 10.0.0.2
			}`,
			wantErr: "api is required",
		},
		{
			name: "missing target",
			input: `traefik {
				api http://t/api/http/routers
			}`,
			wantErr: "target is required",
		},
		{
			name: "api with no argument",
			input: `traefik {
				api
				target 10.0.0.2
			}`,
			wantErr: "Wrong argument count",
		},
		{
			name: "target with no argument",
			input: `traefik {
				api http://t/api/http/routers
				target
			}`,
			wantErr: "Wrong argument count",
		},
		{
			name: "empty block",
			input: `traefik {
			}`,
			wantErr: "api is required",
		},
		{
			name:    "bare directive with no block",
			input:   `traefik`,
			wantErr: "api is required",
		},

		// Malformed values.
		{
			name: "api is not a url",
			input: `traefik {
				api    ::nope
				target 10.0.0.2
			}`,
			wantErr: "is not a valid URL",
		},
		{
			name: "api given as host:port is rejected",
			input: `traefik {
				api    10.0.0.1:8080/api/http/routers
				target 10.0.0.2
			}`,
			wantErr: "is not a valid URL",
		},
		{
			name: "api has no scheme",
			input: `traefik {
				api    traefik.example.com/api/http/routers
				target 10.0.0.2
			}`,
			wantErr: "http or https scheme",
		},
		{
			name: "api has extra arguments",
			input: `traefik {
				api    http://t/api/http/routers extra
				target 10.0.0.2
			}`,
			wantErr: "Wrong argument count",
		},
		{
			name: "api has an unsupported scheme",
			input: `traefik {
				api    ftp://10.0.0.1/api/http/routers
				target 10.0.0.2
			}`,
			wantErr: "http or https scheme",
		},
		{
			name: "target is not an ip",
			input: `traefik {
				api    http://t/api/http/routers
				target traefik.example.com
			}`,
			wantErr: "not an IP address",
		},
		{
			name: "target is ipv6",
			input: `traefik {
				api    http://t/api/http/routers
				target 2001:db8::1
			}`,
			wantErr: "only A records are served",
		},
		{
			name: "interval is not a duration",
			input: `traefik {
				api      http://t/api/http/routers
				target   10.0.0.2
				interval 30
			}`,
			wantErr: "not a duration",
		},
		{
			name: "interval below the minimum",
			input: `traefik {
				api      http://t/api/http/routers
				target   10.0.0.2
				interval 100ms
			}`,
			wantErr: "below the 1s minimum",
		},
		{
			name: "timeout is not a duration",
			input: `traefik {
				api     http://t/api/http/routers
				target  10.0.0.2
				timeout soon
			}`,
			wantErr: "not a duration",
		},
		{
			name: "timeout of zero",
			input: `traefik {
				api     http://t/api/http/routers
				target  10.0.0.2
				timeout 0s
			}`,
			wantErr: "must be positive",
		},
		{
			name: "ttl is not a number",
			input: `traefik {
				api    http://t/api/http/routers
				target 10.0.0.2
				ttl    60s
			}`,
			wantErr: "not a number of seconds",
		},
		{
			name: "ttl above the maximum",
			input: `traefik {
				api    http://t/api/http/routers
				target 10.0.0.2
				ttl    999999999
			}`,
			wantErr: "exceeds the maximum",
		},
		{
			name: "unknown option",
			input: `traefik {
				api      http://t/api/http/routers
				target   10.0.0.2
				fallback 8.8.8.8
			}`,
			wantErr: `unknown option "fallback"`,
		},
		{
			name: "argument on the directive itself",
			input: `traefik example.com {
				api    http://t/api/http/routers
				target 10.0.0.2
			}`,
			wantErr: "Wrong argument count",
		},
		{
			name: "two blocks in one server block",
			input: `traefik {
				api    http://t/api/http/routers
				target 10.0.0.2
			}
			traefik {
				api    http://u/api/http/routers
				target 10.0.0.3
			}`,
			wantErr: "only be configured once",
		},

		// Zone scoping.
		{
			name: "root zone is refused",
			input: `traefik {
				api    http://t/api/http/routers
				target 10.0.0.2
			}`,
			zones:   []string{"."},
			wantErr: "refusing to serve the root zone",
		},
		{
			name: "missing zone is refused",
			input: `traefik {
				api    http://t/api/http/routers
				target 10.0.0.2
			}`,
			zones:   []string{},
			wantErr: "no zone in the server block",
		},
		{
			// What a non-numeric DNS_PORT produces: CoreDNS cannot parse the
			// port, keeps the whole string as the zone name, and falls back to
			// listening on 53.
			name: "zone carrying an unparsed port is refused",
			input: `traefik {
				api    http://t/api/http/routers
				target 10.0.0.2
			}`,
			zones:   []string{"internal.example.com:notaport."},
			wantErr: "contains a colon",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := caddy.NewTestController("dns", tc.input)
			c.ServerBlockKeys = []string{"internal.example.com."}
			if tc.zones != nil {
				c.ServerBlockKeys = tc.zones
			}

			got, err := parse(c)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got none", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %q", tc.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("expected a handler, got nil")
			}
			if got.cfg.api != tc.wantAPI {
				t.Errorf("api = %q, want %q", got.cfg.api, tc.wantAPI)
			}
			if got.cfg.target.String() != tc.wantTarget {
				t.Errorf("target = %q, want %q", got.cfg.target, tc.wantTarget)
			}
			if got.cfg.interval != tc.wantInterval {
				t.Errorf("interval = %s, want %s", got.cfg.interval, tc.wantInterval)
			}
			if got.cfg.ttl != tc.wantTTL {
				t.Errorf("ttl = %d, want %d", got.cfg.ttl, tc.wantTTL)
			}
			if got.cfg.timeout != tc.wantTimeout {
				t.Errorf("timeout = %s, want %s", got.cfg.timeout, tc.wantTimeout)
			}
		})
	}
}

func TestSetupRegistersHandler(t *testing.T) {
	c := caddy.NewTestController("dns", `traefik {
		api    http://10.0.0.1:8080/api/http/routers
		target 10.0.0.2
	}`)
	c.ServerBlockKeys = []string{"internal.example.com."}

	if err := setup(c); err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func TestSetupReportsPluginName(t *testing.T) {
	c := caddy.NewTestController("dns", `traefik {
		target 10.0.0.2
	}`)
	c.ServerBlockKeys = []string{"internal.example.com."}

	err := setup(c)
	if err == nil {
		t.Fatal("expected an error for the missing api option")
	}
	// plugin.Error prefixes so the operator sees which plugin refused to start.
	if !strings.Contains(err.Error(), "plugin/traefik") {
		t.Errorf("error %q should name the plugin", err)
	}
}
