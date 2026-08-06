package traefik

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/dnstest"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/plugin/test"
	"github.com/miekg/dns"
)

const (
	testZone   = "internal.example.com."
	testTarget = "10.0.0.10"
	testNSName = "ns.dns.internal.example.com."
	// The address of the server itself, which is not the Traefik target.
	testSelf = "10.0.0.20"
)

// Several tests deliberately drive poll failures and record churn, both of
// which the plugin is supposed to be loud about. Silence them so a real test
// failure is visible in the output.
func TestMain(m *testing.M) {
	clog.Discard()
	os.Exit(m.Run())
}

func testHandler() *Traefik {
	// nameservers is pinned rather than detected: the addresses of the machine
	// running the tests are not a stable fixture.
	return New([]string{testZone}, config{
		api:         "http://traefik.invalid/api/http/routers",
		target:      net.ParseIP(testTarget).To4(),
		interval:    defaultInterval,
		ttl:         defaultTTL,
		timeout:     defaultTimeout,
		nameservers: []net.IP{net.ParseIP(testSelf).To4()},
	})
}

func loaded(t *Traefik, names ...string) *Traefik {
	byName := make(map[string]net.IP, len(names))
	for _, n := range names {
		byName[n] = t.cfg.target
	}
	t.current.Store(&records{byName: byName, serial: 1})
	return t
}

func TestServeDNS(t *testing.T) {
	tests := []struct {
		name        string
		qname       string
		qtype       uint16
		wantRcode   int
		wantAnswer  []string // rr.String() prefixes are compared field by field below
		wantAuthSOA bool
	}{
		{
			name:       "known name, A",
			qname:      "app.internal.example.com.",
			qtype:      dns.TypeA,
			wantRcode:  dns.RcodeSuccess,
			wantAnswer: []string{testTarget},
		},
		{
			name:       "known name, uppercase query",
			qname:      "APP.Internal.Example.Com.",
			qtype:      dns.TypeA,
			wantRcode:  dns.RcodeSuccess,
			wantAnswer: []string{testTarget},
		},
		{
			name:        "known name, AAAA is NODATA not NXDOMAIN",
			qname:       "app.internal.example.com.",
			qtype:       dns.TypeAAAA,
			wantRcode:   dns.RcodeSuccess,
			wantAuthSOA: true,
		},
		{
			name:        "known name, TXT is NODATA",
			qname:       "app.internal.example.com.",
			qtype:       dns.TypeTXT,
			wantRcode:   dns.RcodeSuccess,
			wantAuthSOA: true,
		},
		{
			name:        "unknown name in zone is NXDOMAIN with SOA",
			qname:       "nope.internal.example.com.",
			qtype:       dns.TypeA,
			wantRcode:   dns.RcodeNameError,
			wantAuthSOA: true,
		},
		{
			name:        "unknown name, AAAA is also NXDOMAIN",
			qname:       "nope.internal.example.com.",
			qtype:       dns.TypeAAAA,
			wantRcode:   dns.RcodeNameError,
			wantAuthSOA: true,
		},
		{
			name:       "zone apex SOA is answered",
			qname:      testZone,
			qtype:      dns.TypeSOA,
			wantRcode:  dns.RcodeSuccess,
			wantAnswer: []string{""}, // presence checked, contents below
		},
		{
			name:        "zone apex A is NODATA, not NXDOMAIN",
			qname:       testZone,
			qtype:       dns.TypeA,
			wantRcode:   dns.RcodeSuccess,
			wantAuthSOA: true,
		},
		{
			// The name the apex NS RRset and the SOA MNAME both point at. It
			// resolves to this server, not to the Traefik target, and exists
			// without any router claiming it.
			name:       "nameserver name resolves to this server",
			qname:      testNSName,
			qtype:      dns.TypeA,
			wantRcode:  dns.RcodeSuccess,
			wantAnswer: []string{testSelf},
		},
		{
			name:        "nameserver name AAAA is NODATA, not NXDOMAIN",
			qname:       testNSName,
			qtype:       dns.TypeAAAA,
			wantRcode:   dns.RcodeSuccess,
			wantAuthSOA: true,
		},
		{
			name:        "a name below the nameserver name is still NXDOMAIN",
			qname:       "sub." + testNSName,
			qtype:       dns.TypeA,
			wantRcode:   dns.RcodeNameError,
			wantAuthSOA: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := loaded(testHandler(), "app.internal.example.com.", "web.internal.example.com.")

			r := new(dns.Msg)
			r.SetQuestion(tc.qname, tc.qtype)
			rec := dnstest.NewRecorder(&test.ResponseWriter{})

			rcode, err := h.ServeDNS(context.Background(), rec, r)
			if err != nil {
				t.Fatalf("ServeDNS: %v", err)
			}
			// A written response is signalled by returning success; the rcode
			// the client sees is the one on the message.
			if rcode != dns.RcodeSuccess {
				t.Fatalf("returned rcode = %d, want %d", rcode, dns.RcodeSuccess)
			}
			if rec.Msg == nil {
				t.Fatal("no response written")
			}
			if rec.Msg.Rcode != tc.wantRcode {
				t.Errorf("message rcode = %s, want %s", dns.RcodeToString[rec.Msg.Rcode], dns.RcodeToString[tc.wantRcode])
			}
			if !rec.Msg.Authoritative {
				t.Error("response is not authoritative")
			}

			if len(tc.wantAnswer) != len(rec.Msg.Answer) {
				t.Fatalf("answer count = %d, want %d (%v)", len(rec.Msg.Answer), len(tc.wantAnswer), rec.Msg.Answer)
			}
			for i, want := range tc.wantAnswer {
				if want == "" {
					continue
				}
				a, ok := rec.Msg.Answer[i].(*dns.A)
				if !ok {
					t.Fatalf("answer %d is %T, want *dns.A", i, rec.Msg.Answer[i])
				}
				if a.A.String() != want {
					t.Errorf("answer %d = %s, want %s", i, a.A, want)
				}
				if a.Hdr.Ttl != defaultTTL {
					t.Errorf("answer %d TTL = %d, want %d", i, a.Hdr.Ttl, defaultTTL)
				}
			}

			if tc.wantAuthSOA {
				if len(rec.Msg.Ns) != 1 {
					t.Fatalf("authority section has %d records, want 1 SOA", len(rec.Msg.Ns))
				}
				soa, ok := rec.Msg.Ns[0].(*dns.SOA)
				if !ok {
					t.Fatalf("authority record is %T, want *dns.SOA", rec.Msg.Ns[0])
				}
				if soa.Hdr.Name != testZone {
					t.Errorf("SOA name = %s, want %s", soa.Hdr.Name, testZone)
				}
				// The negative caching TTL: without it, resolvers pick their own.
				if soa.Minttl != defaultTTL {
					t.Errorf("SOA minttl = %d, want %d", soa.Minttl, defaultTTL)
				}
			}
		})
	}
}

// A zone with an SOA but no NS RRset strands any client that does zone-cut
// discovery: SOA, then NS, then resolve the NS name. Each of those three steps
// has to work, and the third one cannot depend on a Traefik router existing.
func TestServeDNSApexNS(t *testing.T) {
	h := loaded(testHandler(), "app.internal.example.com.")

	r := new(dns.Msg)
	r.SetQuestion(testZone, dns.TypeNS)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})

	if _, err := h.ServeDNS(context.Background(), rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if rec.Msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %s, want NOERROR", dns.RcodeToString[rec.Msg.Rcode])
	}
	if !rec.Msg.Authoritative {
		t.Error("NS answer is not authoritative")
	}

	if len(rec.Msg.Answer) != 1 {
		t.Fatalf("answer has %d records, want 1 NS (%v)", len(rec.Msg.Answer), rec.Msg.Answer)
	}
	ns, ok := rec.Msg.Answer[0].(*dns.NS)
	if !ok {
		t.Fatalf("answer is %T, want *dns.NS", rec.Msg.Answer[0])
	}
	if ns.Hdr.Name != testZone {
		t.Errorf("NS owner = %s, want %s", ns.Hdr.Name, testZone)
	}
	if ns.Ns != testNSName {
		t.Errorf("NS target = %s, want %s", ns.Ns, testNSName)
	}

	// Glue, so the client does not have to come back for the address.
	if len(rec.Msg.Extra) != 1 {
		t.Fatalf("additional section has %d records, want 1 A (%v)", len(rec.Msg.Extra), rec.Msg.Extra)
	}
	glue, ok := rec.Msg.Extra[0].(*dns.A)
	if !ok {
		t.Fatalf("additional record is %T, want *dns.A", rec.Msg.Extra[0])
	}
	if glue.Hdr.Name != testNSName || glue.A.String() != testSelf {
		t.Errorf("glue = %s -> %s, want %s -> %s", glue.Hdr.Name, glue.A, testNSName, testSelf)
	}
}

// The SOA names a nameserver; the NS RRset names a nameserver. If those two ever
// disagree, a client that follows the MNAME and a client that follows the NS
// end up at different places, and one of them is wrong.
func TestSOAMNameMatchesNS(t *testing.T) {
	h := loaded(testHandler(), "app.internal.example.com.")

	soa, ok := h.soa(testZone, 1).(*dns.SOA)
	if !ok {
		t.Fatalf("soa() returned %T", h.soa(testZone, 1))
	}
	ns, ok := h.ns(testZone).(*dns.NS)
	if !ok {
		t.Fatalf("ns() returned %T", h.ns(testZone))
	}
	if soa.Ns != ns.Ns {
		t.Errorf("SOA MNAME = %s, NS target = %s; they must be the same name", soa.Ns, ns.Ns)
	}
}

// A router is free to claim any hostname, including this one. The zone's own
// infrastructure has to win: pointing the NS at the Traefik listener would name
// a host that does not answer DNS at all.
func TestNameserverNameBeatsARouter(t *testing.T) {
	h := loaded(testHandler(), testNSName)

	r := new(dns.Msg)
	r.SetQuestion(testNSName, dns.TypeA)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})

	if _, err := h.ServeDNS(context.Background(), rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if len(rec.Msg.Answer) != 1 {
		t.Fatalf("answer has %d records, want 1", len(rec.Msg.Answer))
	}
	if got := rec.Msg.Answer[0].(*dns.A).A.String(); got != testSelf {
		t.Errorf("nameserver name resolved to %s, want %s (the server, not the target)", got, testSelf)
	}
}

// With no address to publish, the NS RRset is still served - a zone without one
// is malformed - but the name it points at answers NODATA rather than NXDOMAIN,
// which is what an unreachable-but-existing nameserver looks like.
func TestNameserverWithNoAddresses(t *testing.T) {
	h := loaded(testHandler(), "app.internal.example.com.")
	h.nsIPs = nil

	for _, tc := range []struct {
		qname     string
		qtype     uint16
		wantRcode int
		wantCount int
	}{
		{testZone, dns.TypeNS, dns.RcodeSuccess, 1},
		{testNSName, dns.TypeA, dns.RcodeSuccess, 0},
	} {
		r := new(dns.Msg)
		r.SetQuestion(tc.qname, tc.qtype)
		rec := dnstest.NewRecorder(&test.ResponseWriter{})

		if _, err := h.ServeDNS(context.Background(), rec, r); err != nil {
			t.Fatalf("ServeDNS: %v", err)
		}
		if rec.Msg.Rcode != tc.wantRcode {
			t.Errorf("%s %s: rcode = %s, want %s", tc.qname, dns.TypeToString[tc.qtype],
				dns.RcodeToString[rec.Msg.Rcode], dns.RcodeToString[tc.wantRcode])
		}
		if len(rec.Msg.Answer) != tc.wantCount {
			t.Errorf("%s %s: answer has %d records, want %d", tc.qname, dns.TypeToString[tc.qtype],
				len(rec.Msg.Answer), tc.wantCount)
		}
		if len(rec.Msg.Extra) != 0 {
			t.Errorf("%s %s: additional section has %d records, want none",
				tc.qname, dns.TypeToString[tc.qtype], len(rec.Msg.Extra))
		}
	}
}

func TestInterfaceIPv4s(t *testing.T) {
	// Whatever this machine has, the contract holds: routable IPv4 only, and
	// never loopback, which would name this server only to itself.
	for _, ip := range interfaceIPv4s() {
		if ip.To4() == nil {
			t.Errorf("%s is not IPv4", ip)
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			t.Errorf("%s is not an address a client can reach this server on", ip)
		}
	}
}

func TestServeDNSColdStartIsServfail(t *testing.T) {
	h := testHandler() // no successful poll yet

	if h.Ready() {
		t.Error("Ready() is true before the first successful poll")
	}

	r := new(dns.Msg)
	r.SetQuestion("app.internal.example.com.", dns.TypeA)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})

	rcode, err := h.ServeDNS(context.Background(), rec, r)
	if err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	// SERVFAIL rather than NXDOMAIN: a negative answer during a cold start
	// would be cached upstream and outlive the outage.
	if rcode != dns.RcodeServerFailure {
		t.Errorf("rcode = %d, want SERVFAIL (%d)", rcode, dns.RcodeServerFailure)
	}
	if rec.Msg != nil {
		t.Error("a response was written; SERVFAIL should be left to the chain")
	}
}

func TestServeDNSOutOfZoneFallsThrough(t *testing.T) {
	h := loaded(testHandler(), "app.internal.example.com.")
	called := false
	h.Next = plugin.HandlerFunc(func(_ context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
		called = true
		return dns.RcodeSuccess, nil
	})

	r := new(dns.Msg)
	r.SetQuestion("www.example.com.", dns.TypeA)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})

	if _, err := h.ServeDNS(context.Background(), rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if !called {
		t.Error("out-of-zone query did not reach the next plugin")
	}
}

func TestReadyAfterSuccessfulPoll(t *testing.T) {
	h := testHandler()
	if h.Ready() {
		t.Fatal("Ready() is true before any poll")
	}
	loaded(h, "app.internal.example.com.")
	if !h.Ready() {
		t.Error("Ready() is false after a successful poll")
	}
}

func TestRecordsFrom(t *testing.T) {
	h := testHandler()

	got := h.recordsFrom([]router{
		{Name: "app@docker", Rule: "Host(`app.internal.example.com`)", Status: "enabled"},
		{Name: "web@docker", Rule: "Host(`web.internal.example.com`) && PathPrefix(`/`)", Status: "enabled"},
		{Name: "broken@docker", Rule: "Host(`broken.internal.example.com`)", Status: "disabled"},
		{Name: "public@docker", Rule: "Host(`www.example.com`)", Status: "enabled"},
		{Name: "api@internal", Rule: "PathPrefix(`/api`)", Status: "enabled"},
		{Name: "legacy@file", Rule: "Host(`legacy.internal.example.com`)"}, // no status reported
	})

	want := map[string]net.IP{
		"app.internal.example.com.":    h.cfg.target,
		"web.internal.example.com.":    h.cfg.target,
		"legacy.internal.example.com.": h.cfg.target,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recordsFrom()\n got: %v\nwant: %v", got, want)
	}
}

func TestFetch(t *testing.T) {
	const body = `[
		{"name":"app@docker","rule":"Host(` + "`app.internal.example.com`" + `)","status":"enabled"},
		{"name":"public@docker","rule":"Host(` + "`www.example.com`" + `)","status":"enabled"}
	]`

	tests := []struct {
		name      string
		status    int
		body      string
		wantErr   bool
		wantNames []string
	}{
		{name: "ok", status: http.StatusOK, body: body, wantNames: []string{"app.internal.example.com."}},
		{name: "empty list", status: http.StatusOK, body: `[]`, wantNames: nil},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{}`, wantErr: true},
		{name: "not found", status: http.StatusNotFound, body: `404`, wantErr: true},
		// What a forward-auth login page looks like from here.
		{name: "html instead of json", status: http.StatusOK, body: `<!doctype html><title>Sign in</title>`, wantErr: true},
		{name: "truncated json", status: http.StatusOK, body: `[{"rule":`, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			h := testHandler()
			h.cfg.api = srv.URL

			got, err := h.fetch(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if len(got) != len(tc.wantNames) {
				t.Fatalf("got %d records %v, want %d", len(got), got, len(tc.wantNames))
			}
			for _, name := range tc.wantNames {
				if _, ok := got[name]; !ok {
					t.Errorf("record %s missing from %v", name, got)
				}
			}
		})
	}
}

func TestFetchTimesOut(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	h := testHandler()
	h.cfg.api = srv.URL
	h.cfg.timeout = 50 * time.Millisecond
	h.client.Timeout = h.cfg.timeout

	if _, err := h.fetch(context.Background()); err == nil {
		t.Error("expected a timeout error, got none")
	}
}

func TestPollFailureRetainsPreviousRecords(t *testing.T) {
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`[{"name":"app@docker","rule":"Host(` + "`app.internal.example.com`" + `)","status":"enabled"}]`))
	}))
	defer srv.Close()

	h := testHandler()
	h.cfg.api = srv.URL

	h.poll(context.Background())
	before := h.current.Load()
	if before == nil || len(before.byName) != 1 {
		t.Fatalf("first poll produced %v, want 1 record", before)
	}

	// The API going away is not evidence that every service went away.
	fail = true
	h.poll(context.Background())

	after := h.current.Load()
	if after != before {
		t.Fatal("a failed poll replaced the record snapshot")
	}
	if h.failures.Load() != 1 {
		t.Errorf("consecutive failures = %d, want 1", h.failures.Load())
	}

	// Names still resolve throughout the outage.
	r := new(dns.Msg)
	r.SetQuestion("app.internal.example.com.", dns.TypeA)
	rec := dnstest.NewRecorder(&test.ResponseWriter{})
	if _, err := h.ServeDNS(context.Background(), rec, r); err != nil {
		t.Fatalf("ServeDNS: %v", err)
	}
	if rec.Msg == nil || len(rec.Msg.Answer) != 1 {
		t.Fatal("name stopped resolving while the API was down")
	}

	// And the counter resets once it comes back.
	fail = false
	h.poll(context.Background())
	if h.failures.Load() != 0 {
		t.Errorf("consecutive failures = %d after recovery, want 0", h.failures.Load())
	}
}

func TestPollColdStartFailureLeavesHandlerUnready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	h := testHandler()
	h.cfg.api = srv.URL
	h.poll(context.Background())

	if h.Ready() {
		t.Error("Ready() is true after a failed cold-start poll")
	}
	if h.current.Load() != nil {
		t.Error("a failed cold-start poll produced a snapshot")
	}
}

// The snapshot swap has to be safe against queries in flight. Run with -race.
func TestConcurrentPollAndServe(t *testing.T) {
	names := []string{"a", "b", "c", "d"}
	var i atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Return a different router set each time so the map is genuinely
		// replaced rather than rewritten to the same contents.
		n := names[int(i.Add(1))%len(names)]
		w.Write([]byte(`[{"name":"x@docker","rule":"Host(` + "`" + n + `.internal.example.com` + "`" + `)","status":"enabled"}]`))
	}))
	defer srv.Close()

	h := testHandler()
	h.cfg.api = srv.URL
	h.poll(context.Background())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.poll(context.Background())
			}
		}
	}()

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				r := new(dns.Msg)
				r.SetQuestion("a.internal.example.com.", dns.TypeA)
				rec := dnstest.NewRecorder(&test.ResponseWriter{})
				if _, err := h.ServeDNS(context.Background(), rec, r); err != nil {
					t.Errorf("ServeDNS: %v", err)
					return
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestRunStopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	h := testHandler()
	h.cfg.api = srv.URL
	h.cfg.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.run(ctx)
		close(done)
	}()

	// A config reload builds a new handler; the old poll loop has to end with it.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after its context was cancelled")
	}
}

func TestDiff(t *testing.T) {
	ip := net.ParseIP(testTarget).To4()
	prev := map[string]net.IP{"a.": ip, "b.": ip}
	next := map[string]net.IP{"b.": ip, "c.": ip}

	added, removed := diff(prev, next)
	if !reflect.DeepEqual(added, []string{"c."}) {
		t.Errorf("added = %v, want [c.]", added)
	}
	if !reflect.DeepEqual(removed, []string{"a."}) {
		t.Errorf("removed = %v, want [a.]", removed)
	}
}

func TestName(t *testing.T) {
	if got := testHandler().Name(); got != pluginName {
		t.Errorf("Name() = %q, want %q", got, pluginName)
	}
}
