# coredns-traefik

A single minimal container that serves authoritative DNS for one internal zone,
with A records derived automatically from Traefik's routers.

It is a CoreDNS [external plugin](plugin/traefik/) compiled into a CoreDNS
binary — one process, configured entirely by environment variables, with no
volume mounts, no `docker.sock`, and no generated files.

```
app.internal.example.com.     60  IN  A  10.0.0.10   # Traefik saw the router
nope.internal.example.com.        NXDOMAIN          # nothing routes that name
```

## Why

Behind a reverse proxy, every fronted service resolves to the *same* address and
Traefik routes by `Host` header. Maintaining those names by hand in the network
DNS is the chore this removes: a new service becomes resolvable as soon as
Traefik sees its labels, and stops resolving when it goes away.

A wildcard record would also do that, with none of the code. It is rejected here
on purpose:

- A dead or mistyped name must return **NXDOMAIN**, not resolve to a proxy that
  answers 404.
- The zone doubles as a live, `dig`-able inventory of what is actually routed.

Discovery is through Traefik's HTTP API rather than container labels, so it sees
Traefik's merged view — file-provider routers included — and never needs access
to the Docker socket.

## Quick start

```yaml
services:
  traefik-dns:
    image: ghcr.io/ryanmelena/coredns-traefik:1.0.0
    restart: unless-stopped
    environment:
      DNS_ZONE:    "internal.example.com"
      TRAEFIK_API: "http://traefik:8080/api/http/routers"
      TARGET_IP:   "10.0.0.10"
    ports:
      - "10.0.0.20:53:5353/udp"
      - "10.0.0.20:53:5353/tcp"
    cap_drop: [ALL]
```

See [docker-compose.example.yml](docker-compose.example.yml) for the annotated
version, or [docker-compose.macvlan.example.yml](docker-compose.macvlan.example.yml)
if the container should hold its own address on the VLAN.

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `DNS_ZONE` | yes | — | The zone served, e.g. `internal.example.com` |
| `TRAEFIK_API` | yes | — | Traefik routers endpoint |
| `TARGET_IP` | yes | — | IPv4 every discovered name resolves to |
| `DNS_PORT` | no | `5353` | DNS listener (udp and tcp) |
| `METRICS_PORT` | no | `9153` | Prometheus metrics |
| `POLL_INTERVAL` | no | `30s` | Time between polls |
| `RECORD_TTL` | no | `60` | Answer TTL, and the negative caching TTL |
| `API_TIMEOUT` | no | `5s` | Per-request HTTP timeout |
| `NS_IP` | no | detected | Address published for `ns.dns.<zone>`; set it when the container is on more than one network |

The three required variables have no defaults on purpose. Unset, they substitute
to nothing and the container refuses to start — a DNS server that comes up
misconfigured is worse than one that does not come up, because it answers
authoritatively with the wrong thing.

## How it behaves

Full details in the [plugin README](plugin/traefik/README.md). The parts that
matter operationally:

- **In-zone misses are NXDOMAIN.** The plugin never forwards. If you add a
  `forward` to the Corefile you defeat the design, and if the upstream is the
  resolver that forwards this zone here, you create a loop.
- **A poll failure keeps the last good records.** The API being briefly
  unreachable is not evidence that every service disappeared.
- **A cold start with the API down is SERVFAIL, not NXDOMAIN.** A negative
  answer there would be cached upstream and outlive the outage.
- **AAAA on a known name is NODATA,** not NXDOMAIN — the name exists, the type
  does not.
- **The apex carries SOA and NS,** and the nameserver name `ns.dns.<zone>`
  resolves to the container itself. Nothing delegates to this server, but a
  client that discovers zones by asking SOA, then NS, then resolving the
  nameserver name needs all three to be there. `go-acme/lego` is one, and
  reports `could not determine authoritative nameservers` when they are not.
  Note that this does not make an ACME DNS-01 client work against a split
  horizon: point it at public resolvers
  (`--certificatesresolvers.<name>.acme.dnschallenge.resolvers` in Traefik) so
  its zone walk finds the public zone rather than this one.

## Ports

Every listening port is an environment variable, because under macvlan or
ipvlan the container owns its own address and there is no publish step to remap
anything — the values below are the ports on the network.

```yaml
environment:
  DNS_PORT:     "53"     # container has its own IP, so bind 53 directly
  METRICS_PORT: "9153"
networks:
  lan:
    ipv4_address: 10.0.0.20
# no ports: section at all
```

That is the shape of
[docker-compose.macvlan.example.yml](docker-compose.macvlan.example.yml). Under
bridge networking the defaults are fine and the host does the remapping.

The DNS default is 5353 rather than 53 so that binding works on any runtime.
Under Docker you can set `DNS_PORT=53` and keep `cap_drop: ALL` — Docker sets
`net.ipv4.ip_unprivileged_port_start=0` inside containers, so the unprivileged
user can bind low ports with no capabilities. That is a Docker default, not a
kernel one: on a runtime that keeps the kernel's 1024, binding below it needs
`cap_add: NET_BIND_SERVICE` or that sysctl set explicitly.

The metrics endpoint binds all interfaces, so on macvlan it is reachable from
the LAN. Firewall it, or move it to a port you filter. It cannot be turned off
by environment variable — the Corefile ships in the image, and substitution can
change a directive's arguments but not remove the directive. Mount your own
Corefile over `/etc/coredns/Corefile` if you want it gone; no mount is
*required*, but nothing stops you from using one.

**A malformed `DNS_PORT` is refused at startup.** It has to be: CoreDNS reads
the server block as `ZONE:PORT`, and when the port will not parse it keeps the
whole string as a *zone name* and falls back to listening on 53. That would
start a server on the wrong port, authoritative for a zone nobody will ever
query. The plugin rejects a zone containing a colon rather than boot into it.

`METRICS_PORT` is not guarded, because it belongs to another plugin's directive
rather than this one. **A malformed `METRICS_PORT` silently disables metrics** —
the server starts and serves DNS correctly, but nothing listens there. If you
change it, confirm the endpoint answers. A failing scrape is itself the alert.

## Monitoring

The failure worth alerting on is not a crash. It is this quietly serving an
empty or stale zone while looking healthy:

```promql
coredns_traefik_records == 0
time() - coredns_traefik_last_success_timestamp_seconds > 300
```

The image is distroless, so the container healthcheck can only prove the binary
is intact. Use the metrics for anything more.

**There is no `/health` or `/ready` endpoint,** which is a deliberate removal
rather than an oversight. Nothing in this topology can consume one: Unbound and
dnsmasq forward by static configuration and poll nothing, there is no
orchestrator running probes, and the distroless image has no shell for a
healthcheck to curl with. Both would also have reported healthy in exactly the
case worth catching — a live process serving a stale zone. Cold-start
correctness does not depend on them either; the SERVFAIL is enforced in
`ServeDNS` regardless of whether anything is watching.

The plugin itself still implements the *ready* interface, so adding `ready` to
a Corefile of your own works and behaves correctly. It is simply not enabled in
the one that ships.

## Prerequisites

The Traefik API must be reachable from this container and **must not be behind
forward-auth** for `GET /api/http/routers` — an auth redirect comes back as HTML
and every poll fails. Check before deploying:

```bash
curl -fsS http://traefik:8080/api/http/routers | head
```

If it is gated, either exempt that path or bind the API to an internal
entrypoint restricted by firewall.

## Building

```bash
docker build -t coredns-traefik:dev .
go test -race ./...
./test/integration.sh coredns-traefik:dev
```

The build clones CoreDNS at the tag in the `COREDNS_VERSION` build arg, inserts
the plugin into `plugin.cfg`, and rebuilds. Plugin order comes from the position
in `plugin.cfg`, not from the Corefile. The build fails rather than shipping an
image whose plugin did not link.

[`test/integration.sh`](test/integration.sh) runs the acceptance criteria
against a stubbed Traefik API: resolution, NXDOMAIN with SOA, NODATA on AAAA,
the apex zone cut (SOA, NS, and a nameserver name that resolves to a server
which answers), discovery and removal within a poll interval, record retention
across an API outage, cold-start SERVFAIL, and refusal to start when
misconfigured.

## Maintenance

The standing cost of this repo is the rebuild obligation: the image pins a
CoreDNS tag and a Go toolchain, and neither picks up security fixes on its own.
CI rebuilds weekly, and Renovate watches `COREDNS_VERSION` and the Go modules.
The table tests are the guard when bumping CoreDNS — the plugin API is stable
but not frozen, so expect the occasional signature fix.

## Scope

Not a general resolver. No recursion, no caching for other zones, no PTR
records, no DNSSEC, no Kubernetes, and it never writes to an external DNS
server.
