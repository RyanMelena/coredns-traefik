# traefik

## Name

*traefik* - serves A records for hostnames discovered from the Traefik HTTP API.

## Description

The *traefik* plugin polls a Traefik router endpoint (`/api/http/routers`),
extracts every hostname named in a `` Host(`…`) `` matcher, keeps the ones
inside the zones of its server block, and answers A queries for them with a
single configured target address — the address of the Traefik listener that
routes them by `Host` header.

The plugin is authoritative for its zones and **never forwards**. A name in the
zone that no router claims returns NXDOMAIN with an SOA in the authority
section, so dead or mistyped names fail as names rather than reaching a reverse
proxy that would answer 404. The zone is therefore also a queryable inventory of
what is currently routed.

Records are held in memory and refreshed on an interval. There is no state on
disk and nothing to mount.

## Syntax

~~~
traefik {
    api      URL
    target   IP
    interval DURATION
    ttl      SECONDS
    timeout  DURATION
}
~~~

* `api` **URL** of the Traefik routers endpoint, e.g.
  `http://traefik:8080/api/http/routers`. Must be http or https. **Required.**
* `target` **IP** — the IPv4 address every discovered name resolves to, i.e. the
  Traefik listener. **Required.**
* `interval` **DURATION** between polls, as a Go duration. Default `30s`,
  minimum `1s`.
* `ttl` **SECONDS** on the answers, and the negative caching TTL of the SOA.
  Default `60`, maximum `604800`.
* `timeout` **DURATION** for one HTTP request to the API. Default `5s`.

Every value is validated when the Corefile is parsed. A missing or malformed
one aborts startup rather than producing a server that answers authoritatively
with the wrong thing.

## Rule parsing

Hostnames come from positive `` Host(`…`) `` matchers only:

| Rule | Names extracted |
|---|---|
| ``Host(`a.example.com`)`` | `a.example.com` |
| ``Host(`a.example.com`, `b.example.com`)`` | both |
| ``Host(`a.example.com`) && PathPrefix(`/api`)`` | `a.example.com` |
| ``Host(`a.example.com`) \|\| Host(`b.example.com`)`` | both |
| ``!Host(`a.example.com`)`` | none — the matcher excludes the name |
| ``HostRegexp(`^.+\.example\.com$`)`` | none — not a finite set of names |
| ``HostSNI(`a.example.com`)`` | none — TCP routers are not polled |
| ``Headers(`X-Forwarded-Host`, `a.example.com`)`` | none — not a host matcher |

Backtick, double-quoted and single-quoted arguments are all accepted. Names are
lowercased and made fully qualified, deduplicated, and dropped unless they fall
inside a zone of the server block. Names containing wildcards or regexp
placeholders are not records and are ignored.

Routers Traefik reports with a status other than `enabled` are skipped, so a
router that failed to load does not resolve.

## Answers

| Query | Response |
|---|---|
| `A` for a known name | The target address, with the configured TTL |
| Any other type for a known name | NOERROR, empty answer, SOA in authority (NODATA) |
| Any type for an unknown name in zone | NXDOMAIN, authoritative, SOA in authority |
| `SOA` at the zone apex | The synthesized SOA |
| Anything out of zone | Passed to the next plugin |
| Any in-zone query before the first successful poll | SERVFAIL |

AAAA for a known name is NODATA rather than NXDOMAIN: the name exists, the type
does not. Answering NXDOMAIN there tells a dual-stack client the name is gone
outright.

The SERVFAIL on cold start is deliberate. Until the first poll succeeds the
plugin does not know what the zone contains, and an NXDOMAIN would be cached by
the resolvers above it and outlive the outage.

The apex SOA is synthesized (`ns.dns.<zone>` / `hostmaster.<zone>`) with a serial
taken from the last successful poll. No NS records are served: the zone is
reached by a forwarding stanza on the resolver in front of it, not by
delegation.

## Poll failures

A failed poll leaves the previous record set in place and logs an error. The API
being briefly unreachable is not evidence that every service disappeared, and
flushing the zone would take internal resolution down with it. Consecutive
failures are counted in the metrics below.

## Ready

The plugin implements the *ready* interface and reports ready only after the
first successful poll, so adding *ready* to a server block using this plugin
behaves correctly.

It is worth knowing what that does and does not buy you. *ready* is a reporting
endpoint: it does not gate DNS serving, and the cold-start behaviour above holds
whether or not it is enabled. The SERVFAIL comes from `ServeDNS` finding no
snapshot loaded, not from readiness state. Enable *ready* if something actually
consumes it — an orchestrator probe, a dependency gate — and leave it out
otherwise. The Corefile shipped in this repo's image leaves it out.

## Metrics

With the *prometheus* plugin enabled:

* `coredns_traefik_records{}` - number of A records currently served.
* `coredns_traefik_last_success_timestamp_seconds{}` - time of the last
  successful poll.
* `coredns_traefik_consecutive_poll_failures{}` - polls failed since the last
  success.
* `coredns_traefik_polls_total{status}` - polls by outcome, `success` or
  `failure`.
* `coredns_traefik_poll_duration_seconds{}` - histogram of poll durations.

The failure worth alerting on is not a crash but a silently empty or stale zone.
`coredns_traefik_records == 0` and a stale
`coredns_traefik_last_success_timestamp_seconds` are the two signals for that.

## Examples

Serve an internal zone from a Traefik instance on the same network:

~~~ corefile
internal.example.com:5353 {
    traefik {
        api      http://traefik:8080/api/http/routers
        target   10.0.0.10
        interval 30s
        ttl      60
    }
    prometheus :9153
    log
    errors
}
~~~

Note the absence of *forward*. Adding one would defeat the point: an in-zone
miss would be sent upstream instead of returning NXDOMAIN, and if the upstream
is the resolver that forwards this zone here, it is a loop.

## Also see

The Traefik API reference:
<https://doc.traefik.io/traefik/operations/api/>. The endpoint must be reachable
from this container and must not sit behind forward-auth — an auth redirect
comes back as HTML and is reported as a poll failure.
