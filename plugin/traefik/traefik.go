// Package traefik serves an authoritative internal zone whose A records are
// derived by polling the Traefik HTTP API. See README.md for the full
// description.
package traefik

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnsutil"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

const pluginName = "traefik"

var log = clog.NewWithPlugin(pluginName)

// config holds the validated Corefile directive. It is immutable once setup
// returns.
type config struct {
	api      string
	target   net.IP
	interval time.Duration
	ttl      uint32
	timeout  time.Duration
}

// records is an immutable snapshot of one successful poll. Snapshots are
// swapped in wholesale so ServeDNS never takes a lock, never blocks behind a
// poll, and never observes a half-built map.
type records struct {
	byName map[string]net.IP
	serial uint32
}

// Traefik is the plugin handler.
type Traefik struct {
	Next  plugin.Handler
	Zones []string

	cfg    config
	client *http.Client

	// current is nil until the first successful poll. That nil is meaningful:
	// it is the difference between "this name does not exist" and "we do not
	// yet know what exists".
	current atomic.Pointer[records]

	// failures counts polls since the last successful one, for metrics and for
	// log rate limiting.
	failures atomic.Int64
}

// New builds a handler from an already validated config.
func New(zones []string, cfg config) *Traefik {
	return &Traefik{
		Zones:  zones,
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.timeout},
	}
}

// Name implements plugin.Handler.
func (t *Traefik) Name() string { return pluginName }

// Ready implements the ready plugin's Readiness interface. The server must not
// be advertised as ready before it can answer, or the first queries after a
// restart get authoritative NXDOMAINs for names that do exist.
func (t *Traefik) Ready() bool { return t.current.Load() != nil }

// ServeDNS implements plugin.Handler.
func (t *Traefik) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	qname := state.Name()

	zone := plugin.Zones(t.Zones).Matches(qname)
	if zone == "" {
		return plugin.NextOrFailure(t.Name(), t.Next, ctx, w, r)
	}

	rec := t.current.Load()
	if rec == nil {
		// Cold start with the API unreachable. SERVFAIL, never NXDOMAIN: a
		// negative answer here would be cached by the resolvers above us and
		// would outlive the outage.
		return dns.RcodeServerFailure, nil
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	ip, known := rec.byName[qname]
	apex := qname == zone

	switch {
	case known && state.QType() == dns.TypeA:
		m.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: t.cfg.ttl},
			A:   ip,
		}}

	case apex && state.QType() == dns.TypeSOA:
		m.Answer = []dns.RR{t.soa(zone, rec.serial)}

	case known || apex:
		// The name exists but this type does not, so NODATA: NOERROR with an
		// empty answer section. AAAA is the case that matters - answering
		// NXDOMAIN there would tell a dual-stack client the name is gone
		// outright, and some resolvers cache that across all types.
		m.Ns = []dns.RR{t.soa(zone, rec.serial)}

	default:
		// In zone, but no such name. Authoritative NXDOMAIN with a SOA so the
		// resolvers above cache the negative answer for a bounded time.
		// Deliberately no fallthrough to any upstream: an in-zone miss that
		// escaped this plugin would loop straight back through the resolver
		// that forwards the zone here.
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{t.soa(zone, rec.serial)}
	}

	w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}

// soa builds the authority record attached to negative and NODATA answers. Its
// Minttl doubles as the negative caching TTL (RFC 2308), so it is deliberately
// the same as the record TTL: a name added in Traefik should become resolvable
// on roughly the same timescale as one that changed.
func (t *Traefik) soa(zone string, serial uint32) dns.RR {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: t.cfg.ttl},
		Ns:      dnsutil.Join("ns.dns", zone),
		Mbox:    dnsutil.Join("hostmaster", zone),
		Serial:  serial,
		Refresh: 7200,
		Retry:   1800,
		Expire:  86400,
		Minttl:  t.cfg.ttl,
	}
}
