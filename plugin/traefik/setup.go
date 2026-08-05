package traefik

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
)

const (
	defaultInterval = 30 * time.Second
	defaultTTL      = uint32(60)
	defaultTimeout  = 5 * time.Second

	// minInterval keeps a typo in POLL_INTERVAL from turning this into a load
	// generator against the Traefik API.
	minInterval = time.Second
	// maxTTL is the practical ceiling on a record TTL (one week).
	maxTTL = uint32(604800)
)

func init() { plugin.Register(pluginName, setup) }

func setup(c *caddy.Controller) error {
	t, err := parse(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	// The poll loop outlives setup but not the server: a config reload builds a
	// new handler, and the old one's goroutine has to go with it.
	ctx, cancel := context.WithCancel(context.Background())
	c.OnStartup(func() error {
		go t.run(ctx)
		return nil
	})
	c.OnShutdown(func() error {
		cancel()
		return nil
	})

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		t.Next = next
		return t
	})

	return nil
}

// parse reads the traefik directive. Every value is validated here, and a bad
// one aborts startup: a DNS server that comes up misconfigured is worse than one
// that refuses to come up, because it answers authoritatively with nonsense.
func parse(c *caddy.Controller) (*Traefik, error) {
	var t *Traefik

	for c.Next() {
		if t != nil {
			return nil, errors.New("traefik may only be configured once per server block")
		}

		if len(c.RemainingArgs()) != 0 {
			return nil, c.ArgErr()
		}

		cfg := config{
			interval: defaultInterval,
			ttl:      defaultTTL,
			timeout:  defaultTimeout,
		}

		for c.NextBlock() {
			option := c.Val()
			value, err := singleArg(c)
			if err != nil {
				return nil, err
			}

			switch option {
			case "api":
				cfg.api, err = parseAPI(value)
			case "target":
				cfg.target, err = parseTarget(value)
			case "interval":
				cfg.interval, err = parseInterval(value)
			case "ttl":
				cfg.ttl, err = parseTTL(value)
			case "timeout":
				cfg.timeout, err = parseTimeout(value)
			default:
				return nil, c.Errf("unknown option %q", option)
			}
			if err != nil {
				return nil, c.Errf("%s: %v", option, err)
			}
		}

		if cfg.api == "" {
			return nil, errors.New("api is required (is TRAEFIK_API set in the environment?)")
		}
		if cfg.target == nil {
			return nil, errors.New("target is required (is TARGET_IP set in the environment?)")
		}
		if cfg.timeout > cfg.interval {
			// Not fatal: polls run one at a time, so a long timeout only
			// stretches the effective interval. Worth saying out loud though,
			// because the interval then is not the one that was configured.
			log.Warningf("Timeout %s exceeds interval %s; a stalled poll will delay the next one", cfg.timeout, cfg.interval)
		}

		zones, err := parseZones(c.ServerBlockKeys)
		if err != nil {
			return nil, err
		}

		t = New(zones, cfg)
	}

	return t, nil
}

// singleArg reads exactly one argument for the current option. An unset
// environment variable substitutes to nothing, so "no argument" is the shape a
// missing TRAEFIK_API or TARGET_IP actually arrives in.
func singleArg(c *caddy.Controller) (string, error) {
	args := c.RemainingArgs()
	if len(args) != 1 || args[0] == "" {
		return "", c.ArgErr()
	}
	return args[0], nil
}

func parseAPI(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid URL: %w", v, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%q must use the http or https scheme", v)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%q has no host", v)
	}
	if u.Path == "" || u.Path == "/" {
		// Not fatal - the endpoint may be proxied - but pointing at the
		// dashboard root instead of the routers endpoint is a common mistake
		// and produces HTML that only shows up as a decode error later.
		log.Warningf("API URL %q has no path; the routers endpoint is usually /api/http/routers", v)
	}
	return v, nil
}

func parseTarget(v string) (net.IP, error) {
	ip := net.ParseIP(v)
	if ip == nil {
		return nil, fmt.Errorf("%q is not an IP address", v)
	}
	// The plugin serves A records only; an IPv6 target would silently produce a
	// zone where every name resolves to nothing.
	if ip.To4() == nil {
		return nil, fmt.Errorf("%q is IPv6, but only A records are served", v)
	}
	return ip.To4(), nil
}

func parseInterval(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration: %w", v, err)
	}
	if d < minInterval {
		return 0, fmt.Errorf("%s is below the %s minimum", d, minInterval)
	}
	return d, nil
}

func parseTimeout(v string) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration: %w", v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive", d)
	}
	return d, nil
}

func parseTTL(v string) (uint32, error) {
	ttl, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number of seconds: %w", v, err)
	}
	if ttl > uint64(maxTTL) {
		return 0, fmt.Errorf("%d exceeds the maximum of %d", ttl, maxTTL)
	}
	return uint32(ttl), nil
}

func parseZones(serverBlockKeys []string) ([]string, error) {
	zones := plugin.OriginsFromArgsOrServerBlock(nil, serverBlockKeys)
	if len(zones) == 0 {
		return nil, errors.New("no zone in the server block (is DNS_ZONE set in the environment?)")
	}
	for _, z := range zones {
		// A colon cannot appear in a domain name, so its presence means CoreDNS
		// read ZONE:PORT, failed to parse the port, and kept the whole string as
		// the zone name - falling back to port 53. That starts a server which
		// listens on the wrong port and is authoritative for a zone nobody will
		// ever query, which is exactly the silent misconfiguration this plugin
		// refuses to boot into.
		if strings.Contains(z, ":") {
			return nil, fmt.Errorf("zone %q contains a colon, so the port after it is not a valid port number (is DNS_PORT numeric?)", z)
		}
		// Serving the root zone would put this plugin in front of all
		// resolution and blackhole the internet, since it never forwards.
		if z == "." {
			return nil, errors.New("refusing to serve the root zone; scope the server block to an internal zone")
		}
	}
	return zones, nil
}
