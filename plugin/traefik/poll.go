package traefik

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// maxBodyBytes caps how much of the API response we will read. A forward-auth
// login page or a misaddressed endpoint can return an unbounded stream, and
// this process has no business buffering it.
const maxBodyBytes = 32 << 20

// Router statuses Traefik reports. "enabled" and "warning" routers are both
// being served - a warning is a non-critical error, such as one of several
// entrypoints not existing - so both resolve. Routers that failed to load carry
// "disabled" plus an error list, and must not be resolvable.
const (
	statusEnabled = "enabled"
	statusWarning = "warning"
)

// The routers endpoint is paginated: Traefik returns at most per_page routers
// (default 100), sorted by name, and names the following page in the
// X-Next-Page header, which is "1" on the last page. Reading only the first
// page silently drops every router past the hundredth - and since a new router
// shifts everything after it down by one, starting a container can push an
// existing name off the end, too.
const (
	perPage        = 1000
	nextPageHeader = "X-Next-Page"
	// maxPages bounds the walk against an endpoint that never stops naming a
	// next page. At perPage routers a page that is 100,000 routers, far beyond
	// any real install.
	maxPages = 100
)

// router is the subset of Traefik's /api/http/routers entries we care about.
type router struct {
	Name   string `json:"name"`
	Rule   string `json:"rule"`
	Status string `json:"status"`
}

// run polls until ctx is cancelled. The first poll happens immediately and
// off the startup path: a Traefik that is slow or down at boot must not stop
// CoreDNS from starting, it must only keep it from reporting ready.
func (t *Traefik) run(ctx context.Context) {
	t.poll(ctx)

	ticker := time.NewTicker(t.cfg.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.poll(ctx)
		}
	}
}

// poll refreshes the record set from Traefik. A failed poll leaves the previous
// snapshot in place: the API blipping is not evidence that every service went
// away, and flushing the zone would take the whole network's internal
// resolution down with it.
func (t *Traefik) poll(ctx context.Context) {
	start := time.Now()
	byName, err := t.fetch(ctx)
	pollDuration.Observe(time.Since(start).Seconds())

	if err != nil {
		n := t.failures.Add(1)
		pollFailures.Set(float64(n))
		pollCount.WithLabelValues("failure").Inc()
		log.Errorf("Poll %d of %s failed, serving last known records: %v", n, t.cfg.api, err)
		return
	}

	t.failures.Store(0)
	pollFailures.Set(0)
	pollCount.WithLabelValues("success").Inc()

	now := time.Now()
	prev := t.current.Load()
	t.current.Store(&records{byName: byName, serial: uint32(now.Unix())})

	recordCount.Set(float64(len(byName)))
	lastSuccess.Set(float64(now.Unix()))

	if len(byName) == 0 {
		log.Warning("Poll succeeded but matched no routers in the served zone; every in-zone query will now return NXDOMAIN")
	}
	logChanges(prev, byName)
}

// fetch retrieves every page of routers and reduces them to a record map. The
// timeout covers the whole walk, not each page, so a poll's worst case stays
// what the configuration says it is.
func (t *Traefik) fetch(ctx context.Context) (map[string]net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, t.cfg.timeout)
	defer cancel()

	var routers []router
	for page := 1; ; {
		batch, next, err := t.fetchPage(ctx, page)
		if err != nil {
			return nil, err
		}
		routers = append(routers, batch...)

		// Stop on the last page ("1"), on a missing header (something that is
		// not Traefik, or a proxy that strips it, and so is not paginating),
		// and on anything that does not move forward, which would loop.
		if next <= page {
			break
		}
		if next > maxPages {
			return nil, fmt.Errorf("router list runs past %d pages of %d", maxPages, perPage)
		}
		page = next
	}

	return t.recordsFrom(routers), nil
}

// fetchPage retrieves one page of routers, and the page number Traefik says
// comes next (0 if it did not say).
func (t *Traefik) fetchPage(ctx context.Context, page int) ([]router, int, error) {
	u, err := url.Parse(t.cfg.api)
	if err != nil {
		return nil, 0, err
	}
	q := u.Query()
	q.Set("per_page", strconv.Itoa(perPage))
	q.Set("page", strconv.Itoa(page))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("unexpected status %s", resp.Status)
	}

	var routers []router
	// A decode failure here most often means the API is behind forward-auth and
	// we were handed a login page, so say so rather than reporting raw JSON
	// noise.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&routers); err != nil {
		return nil, 0, fmt.Errorf("response is not a Traefik router list (is the API behind auth?): %w", err)
	}

	next, _ := strconv.Atoi(resp.Header.Get(nextPageHeader))
	return routers, next, nil
}

// recordsFrom maps every in-zone hostname found in the routers to the target IP.
func (t *Traefik) recordsFrom(routers []router) map[string]net.IP {
	byName := make(map[string]net.IP, len(routers))
	for _, r := range routers {
		// Traefik omits status on some provider outputs; treat only an
		// explicit status other than a served one as a reason to skip.
		switch r.Status {
		case "", statusEnabled, statusWarning:
		default:
			continue
		}
		for _, host := range hostsInZones(r.Rule, t.Zones) {
			byName[host] = t.cfg.target
		}
	}
	return byName
}

// logChanges reports what appeared and disappeared. The zone is a live service
// inventory, so a record vanishing is worth a line in the log even though it is
// not an error.
func logChanges(prev *records, next map[string]net.IP) {
	if prev == nil {
		log.Infof("Serving %d records", len(next))
		return
	}

	added, removed := diff(prev.byName, next)
	if len(added) == 0 && len(removed) == 0 {
		return
	}
	log.Infof("Records changed (%d total): added %v, removed %v", len(next), added, removed)
}

func diff(prev, next map[string]net.IP) (added, removed []string) {
	for name := range next {
		if _, ok := prev[name]; !ok {
			added = append(added, name)
		}
	}
	for name := range prev {
		if _, ok := next[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}
