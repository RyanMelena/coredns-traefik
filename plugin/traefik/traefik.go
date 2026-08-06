// Package traefik serves an authoritative internal zone whose A records are
// derived by polling the Traefik HTTP API. See README.md for the full
// description.
package traefik

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"slices"
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

	// nameservers are the addresses published for the zone's nameserver name.
	// Empty means "work them out from this host's interfaces", which is right
	// for the single-network case and wrong for a container attached to a
	// network its clients cannot reach.
	nameservers []net.IP
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

	// nsIPs are the addresses answered for the zone's nameserver name. They are
	// resolved once at setup: a container's addresses do not change under it,
	// and re-reading them per query would put a syscall on the hot path.
	nsIPs []net.IP

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
	nsIPs := cfg.nameservers
	if len(nsIPs) == 0 {
		nsIPs = interfaceIPv4s()
		if len(nsIPs) == 0 {
			// The NS RRset still gets published: a zone without one is
			// malformed, and a name that resolves to nothing is at least
			// diagnosable. Say so loudly, because the only symptom otherwise is
			// a client that cannot follow the delegation it was just handed.
			log.Warning("No usable IPv4 interface address found; the zone's nameserver name will have no address (set nameserver explicitly)")
		}
	}

	return &Traefik{
		Zones:  zones,
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.timeout},
		nsIPs:  nsIPs,
	}
}

// interfaceIPv4s returns this host's routable IPv4 addresses. They are the
// addresses a client can reach this server on, which is what the zone's
// nameserver name has to resolve to for delegation-following clients to work.
//
// Loopback, link-local and unspecified addresses are excluded: none of them
// name this server from anywhere else. A container attached to more than one
// network yields more than one address, and all of them are published - each is
// genuinely this server, but a client that picks one it cannot route to waits
// for a timeout first, which is the case for setting nameserver explicitly.
func interfaceIPv4s() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Errorf("Cannot enumerate interface addresses: %v", err)
		return nil
	}

	var ips []net.IP
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := n.IP.To4()
		if v4 == nil || v4.IsLoopback() || v4.IsLinkLocalUnicast() || v4.IsUnspecified() {
			continue
		}
		ips = append(ips, v4)
	}

	// Stable order, so the same query does not shuffle its answer between
	// restarts for no reason.
	slices.SortFunc(ips, func(a, b net.IP) int { return bytes.Compare(a, b) })
	return ips
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
	nsname := nsName(zone)

	switch {
	case qname == nsname && state.QType() == dns.TypeA && len(t.nsIPs) > 0:
		// The nameserver name is zone infrastructure rather than a routed
		// service, so it answers from this server's own addresses and does not
		// wait for a router to exist. It is checked before the record map on
		// purpose: a router that claimed this name would otherwise point the
		// zone's NS at the Traefik listener, which does not serve DNS.
		m.Answer = t.nsAddrs(qname)

	case known && state.QType() == dns.TypeA:
		m.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: t.cfg.ttl},
			A:   ip,
		}}

	case apex && state.QType() == dns.TypeNS:
		// Every zone has an NS RRset at its apex. This one is self-referential:
		// nothing delegates to this server - the resolver in front of it
		// forwards - but a client doing zone-cut discovery asks SOA, then NS,
		// then resolves the NS name, and a missing RRset strands it there.
		m.Answer = []dns.RR{t.ns(zone)}
		// Glue in the additional section, so that client does not have to come
		// back for the address it is about to need.
		m.Extra = t.nsAddrs(nsname)

	case apex && state.QType() == dns.TypeSOA:
		m.Answer = []dns.RR{t.soa(zone, rec.serial)}

	case known || apex || qname == nsname:
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

// nsName is the zone's nameserver name. One definition, used for the NS RRset,
// for the SOA's MNAME and for the address lookup, so the three cannot drift
// apart into a zone that names a nameserver it does not answer for.
func nsName(zone string) string { return dnsutil.Join("ns.dns", zone) }

// ns builds the apex NS record.
func (t *Traefik) ns(zone string) dns.RR {
	return &dns.NS{
		Hdr: dns.RR_Header{Name: zone, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: t.cfg.ttl},
		Ns:  nsName(zone),
	}
}

// nsAddrs builds the A records for the nameserver name.
func (t *Traefik) nsAddrs(name string) []dns.RR {
	rrs := make([]dns.RR, 0, len(t.nsIPs))
	for _, ip := range t.nsIPs {
		rrs = append(rrs, &dns.A{
			Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: t.cfg.ttl},
			A:   ip,
		})
	}
	return rrs
}

// soa builds the authority record attached to negative and NODATA answers. Its
// Minttl doubles as the negative caching TTL (RFC 2308), so it is deliberately
// the same as the record TTL: a name added in Traefik should become resolvable
// on roughly the same timescale as one that changed.
func (t *Traefik) soa(zone string, serial uint32) dns.RR {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: t.cfg.ttl},
		Ns:      nsName(zone),
		Mbox:    dnsutil.Join("hostmaster", zone),
		Serial:  serial,
		Refresh: 7200,
		Retry:   1800,
		Expire:  86400,
		Minttl:  t.cfg.ttl,
	}
}
