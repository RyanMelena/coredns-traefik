#!/usr/bin/env bash
#
# Acceptance checks for the built image, run against a stubbed Traefik API.
#
# These cover the behaviours the design turns on, not just "does it start":
# in-zone misses must be NXDOMAIN rather than reaching a proxy, AAAA on a known
# name must be NODATA rather than NXDOMAIN, a poll failure must not flush the
# zone, and a cold start with the API down must be SERVFAIL rather than a
# negative answer that upstream resolvers will cache.
#
# The stub's content is written into the container over `docker exec` rather
# than bind-mounted, so this runs the same way on a developer machine as in CI.
#
# Usage: ./test/integration.sh [image]

set -euo pipefail

IMAGE="${1:-coredns-traefik:ci}"
ZONE="internal.example.com"
TARGET="10.0.0.10"
NET="cdt-it-$$"
STUB="cdt-stub-$$"
DNS="cdt-dns-$$"
COLD="cdt-cold-$$"
PORTED="cdt-port-$$"
LOWPORT="cdt-low-$$"
NSIP="cdt-nsip-$$"
HELPER="alpine:3.22"

failures=0

cleanup() {
  docker rm -f "$DNS" "$COLD" "$PORTED" "$LOWPORT" "$NSIP" "$STUB" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT

check() { # check <description> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s\n         expected: %s\n         actual:   %s\n' "$1" "$2" "$3"
    failures=$((failures + 1))
  fi
}

# Run a helper container on the test network. Never let a non-zero exit abort
# the script: a failed probe is a check result, not a harness error.
helper() { docker run --rm --network "$NET" "$HELPER" sh -c "$1" 2>/dev/null || true; }

dig_in_net() { # dig_in_net <server> <args...>
  local server="$1"; shift
  dig_on_port "$server" 5353 "$@"
}

dig_on_port() { # dig_on_port <server> <port> <args...>
  local server="$1" port="$2"; shift 2
  helper "apk add --no-cache -q bind-tools >/dev/null 2>&1; dig @$server -p $port $*"
}

curl_in_net() { helper "apk add --no-cache -q curl >/dev/null 2>&1; curl -s $1"; }

status_of() { # status_of <server> <name> [type]
  dig_in_net "$1" "$2" "${3:-A}" +noall +comments | sed -n 's/.*status: \([A-Z]*\).*/\1/p'
}

# write_routers [extra-json-object]
write_routers() {
  docker exec -i "$STUB" sh -c 'mkdir -p /www/api/http && cat > /www/api/http/routers' <<EOF
[
  {"name":"app@docker","rule":"Host(\`app.$ZONE\`)","status":"enabled"},
  {"name":"multi@docker","rule":"Host(\`wiki.$ZONE\`, \`docs.$ZONE\`)","status":"enabled"},
  {"name":"combined@docker","rule":"Host(\`web.$ZONE\`) && PathPrefix(\`/\`)","status":"enabled"},
  {"name":"broken@docker","rule":"Host(\`broken.$ZONE\`)","status":"disabled"},
  {"name":"public@docker","rule":"Host(\`www.example.com\`)","status":"enabled"},
  {"name":"regexp@docker","rule":"HostRegexp(\`^.+\\\\.$ZONE\$\`)","status":"enabled"},
  {"name":"negated@docker","rule":"Host(\`ok.$ZONE\`) && !Host(\`blocked.$ZONE\`)","status":"enabled"},
  {"name":"api@internal","rule":"PathPrefix(\`/api\`)","status":"enabled"}${1:-}
]
EOF
}

# Output of a container that is expected to exit non-zero.
output_of() { docker run --rm --network "$NET" "$@" "$IMAGE" 2>&1 || true; }

# Whether the server refuses to come up at all. Some misconfigurations are
# caught by CoreDNS before the plugin's setup runs, so assert on the outcome
# rather than on a message this repo does not own.
refuses_to_start() {
  if docker run --rm --network "$NET" "$@" "$IMAGE" >/dev/null 2>&1; then echo no; else echo yes; fi
}

contains() { case "$2" in *"$1"*) echo yes ;; *) echo no ;; esac; }

echo "Setting up ($IMAGE)"
docker network create "$NET" >/dev/null
docker run -d --name "$STUB" --network "$NET" -w /www "$HELPER" \
  sh -c 'apk add --no-cache -q python3 >/dev/null 2>&1; mkdir -p /www/api/http; python3 -m http.server 8080' >/dev/null

# Wait for the stub to answer before starting anything that polls it.
for _ in $(seq 1 30); do
  write_routers 2>/dev/null && break
  sleep 1
done
for _ in $(seq 1 30); do
  [ -n "$(curl_in_net "http://$STUB:8080/api/http/routers")" ] && break
  sleep 1
done

# A second server pointed at an API that will never resolve, to prove cold-start
# behaviour.
docker run -d --name "$COLD" --network "$NET" --cap-drop ALL \
  -e DNS_ZONE="$ZONE" -e TRAEFIK_API="http://nonexistent:8080/api/http/routers" \
  -e TARGET_IP="$TARGET" -e POLL_INTERVAL=2s -e RECORD_TTL=60 "$IMAGE" >/dev/null

docker run -d --name "$DNS" --network "$NET" --cap-drop ALL --read-only \
  -e DNS_ZONE="$ZONE" -e TRAEFIK_API="http://$STUB:8080/api/http/routers" \
  -e TARGET_IP="$TARGET" -e POLL_INTERVAL=2s -e RECORD_TTL=60 "$IMAGE" >/dev/null

sleep 6

echo
echo "Resolution"
check "known name resolves to the target" \
  "$TARGET" "$(dig_in_net "$DNS" "app.$ZONE" +short)"
check "answer carries the configured TTL" \
  "60" "$(dig_in_net "$DNS" "app.$ZONE" +noall +answer | awk '{print $2}')"
check "answer is authoritative" \
  "yes" "$(contains " aa" "$(dig_in_net "$DNS" "app.$ZONE" +noall +comments)")"
check "both names from a multi-host rule resolve" \
  "$TARGET $TARGET" \
  "$(dig_in_net "$DNS" "wiki.$ZONE" +short) $(dig_in_net "$DNS" "docs.$ZONE" +short)"
check "host combined with another matcher resolves" \
  "$TARGET" "$(dig_in_net "$DNS" "web.$ZONE" +short)"
check "positive host beside a negated one resolves" \
  "$TARGET" "$(dig_in_net "$DNS" "ok.$ZONE" +short)"
check "query is case insensitive" \
  "$TARGET" "$(dig_in_net "$DNS" "APP.$ZONE" +short)"
check "TCP is served as well as UDP" \
  "$TARGET" "$(dig_in_net "$DNS" +tcp "app.$ZONE" +short)"

echo
echo "Negative answers"
check "unknown in-zone name is NXDOMAIN" \
  "NXDOMAIN" "$(status_of "$DNS" "nope.$ZONE")"
check "NXDOMAIN carries an SOA for negative caching" \
  "SOA" "$(dig_in_net "$DNS" "nope.$ZONE" +noall +authority | awk '{print $4}')"
check "disabled router does not resolve" \
  "NXDOMAIN" "$(status_of "$DNS" "broken.$ZONE")"
check "negated host does not resolve" \
  "NXDOMAIN" "$(status_of "$DNS" "blocked.$ZONE")"
check "HostRegexp does not become a wildcard" \
  "NXDOMAIN" "$(status_of "$DNS" "anything.$ZONE")"
check "out-of-zone name is not served" \
  "REFUSED" "$(status_of "$DNS" "www.example.com")"

echo
echo "Types"
check "AAAA on a known name is NOERROR, not NXDOMAIN" \
  "NOERROR" "$(status_of "$DNS" "app.$ZONE" AAAA)"
check "AAAA on a known name has an empty answer" \
  "0" "$(dig_in_net "$DNS" "app.$ZONE" AAAA +noall +comments | sed -n 's/.*ANSWER: \([0-9]*\).*/\1/p')"
check "SOA is answered at the apex" \
  "yes" "$(contains "SOA" "$(dig_in_net "$DNS" "$ZONE" SOA +noall +answer)")"

echo
echo "Zone cut"
# A client doing delegation discovery asks SOA, then NS, then resolves the
# nameserver name. All three have to work or it gives up at whichever step
# comes back empty - which is what go-acme/lego reports as "could not
# determine authoritative nameservers".
check "NS is answered at the apex" \
  "yes" "$(contains "NS" "$(dig_in_net "$DNS" "$ZONE" NS +noall +answer)")"
check "the NS names ns.dns.<zone>" \
  "ns.dns.$ZONE." "$(dig_in_net "$DNS" "$ZONE" NS +short)"
check "the NS name resolves" \
  "yes" "$(contains "." "$(dig_in_net "$DNS" "ns.dns.$ZONE" +short)")"
check "the NS name does not resolve to the Traefik target" \
  "no" "$(contains "$TARGET" "$(dig_in_net "$DNS" "ns.dns.$ZONE" +short)")"
check "the NS answer carries glue" \
  "yes" "$(contains "ns.dns.$ZONE." "$(dig_in_net "$DNS" "$ZONE" NS +noall +additional)")"
check "the SOA MNAME matches the NS" \
  "ns.dns.$ZONE." "$(dig_in_net "$DNS" "$ZONE" SOA +short | awk '{print $1}')"
check "the NS name is reachable at the address it publishes" \
  "$TARGET" "$(dig_in_net "$(dig_in_net "$DNS" "ns.dns.$ZONE" +short | head -1)" "app.$ZONE" +short)"

# Detection publishes every address the container has, which is wrong when only
# one network faces the clients. NS_IP is the way out of that.
docker run -d --name "$NSIP" --network "$NET" --cap-drop ALL --read-only \
  -e DNS_ZONE="$ZONE" -e TRAEFIK_API="http://$STUB:8080/api/http/routers" \
  -e TARGET_IP="$TARGET" -e POLL_INTERVAL=2s -e RECORD_TTL=60 \
  -e NS_IP="10.99.99.99" "$IMAGE" >/dev/null
sleep 6
check "NS_IP overrides the detected address" \
  "10.99.99.99" "$(dig_in_net "$NSIP" "ns.dns.$ZONE" +short)"

echo
echo "Cold start with the API unreachable"
# Asserted through DNS rather than a readiness endpoint: SERVFAIL is what
# clients and the resolvers above actually see, and it is enforced in ServeDNS
# independently of whether anything reports readiness.
check "in-zone query is SERVFAIL, not NXDOMAIN" \
  "SERVFAIL" "$(status_of "$COLD" "app.$ZONE")"
check "cold start does not answer authoritatively" \
  "no" "$(contains " aa" "$(dig_in_net "$COLD" "app.$ZONE" +noall +comments)")"

echo
echo "Discovery"
write_routers ',
  {"name":"new@docker","rule":"Host(`new.'"$ZONE"'`)","status":"enabled"}'
sleep 5
check "a new router resolves within one poll interval" \
  "$TARGET" "$(dig_in_net "$DNS" "new.$ZONE" +short)"

write_routers
sleep 5
check "a removed router returns to NXDOMAIN" \
  "NXDOMAIN" "$(status_of "$DNS" "new.$ZONE")"

echo
echo "API outage"
records_before="$(curl_in_net "http://$DNS:9153/metrics" | awk '/^coredns_traefik_records / {print $2+0}')"
docker stop "$STUB" >/dev/null
sleep 8
check "names still resolve from the last good snapshot" \
  "$TARGET" "$(dig_in_net "$DNS" "app.$ZONE" +short)"
check "consecutive failures are reported in metrics" \
  "yes" "$(curl_in_net "http://$DNS:9153/metrics" |
    awk '/^coredns_traefik_consecutive_poll_failures/ {print ($2+0 > 0) ? "yes" : "no"}')"
check "record count is unchanged during the outage" \
  "$records_before" "$(curl_in_net "http://$DNS:9153/metrics" | awk '/^coredns_traefik_records / {print $2+0}')"

docker start "$STUB" >/dev/null
sleep 8
check "recovers once the API returns" \
  "$TARGET" "$(dig_in_net "$DNS" "app.$ZONE" +short)"

echo
echo "Configurable listen ports"

# start_ported <name> <dns> <metrics>; leaves it running.
start_ported() {
  docker run -d --name "$1" --network "$NET" --cap-drop ALL --read-only \
    -e DNS_ZONE="$ZONE" -e TRAEFIK_API="http://$STUB:8080/api/http/routers" \
    -e TARGET_IP="$TARGET" -e POLL_INTERVAL=2s -e RECORD_TTL=60 \
    -e DNS_PORT="$2" -e METRICS_PORT="$3" \
    "$IMAGE" >/dev/null 2>&1 || true
}

# Everything moved at once, which is the macvlan case: no publish step, so
# these are the ports clients actually reach.
start_ported "$PORTED" 15353 19153
sleep 6
check "serves DNS on the configured DNS_PORT" \
  "$TARGET" "$(dig_on_port "$PORTED" 15353 "app.$ZONE" +short +time=2 +tries=1)"
check "does not answer on the default DNS port" \
  "no" "$(contains "$TARGET" "$(dig_on_port "$PORTED" 5353 "app.$ZONE" +short +time=2 +tries=1)")"
check "metrics on the configured METRICS_PORT" \
  "yes" "$(contains "coredns_traefik_records" "$(curl_in_net "http://$PORTED:19153/metrics")")"
check "nothing left on the default metrics port" \
  "no" "$(contains "coredns_traefik" "$(curl_in_net "--max-time 3 http://$PORTED:9153/metrics")")"

# Docker sets net.ipv4.ip_unprivileged_port_start=0 inside containers, so an
# unprivileged bind of 53 succeeds with no capabilities at all. On a runtime
# that keeps the kernel default of 1024 this would need NET_BIND_SERVICE.
start_ported "$LOWPORT" 53 9153
sleep 6
check "binds 53 with cap_drop ALL under Docker" \
  "$TARGET" "$(dig_on_port "$LOWPORT" 53 "app.$ZONE" +short +time=2 +tries=1)"

echo
echo "Misconfiguration refuses to start"
check "missing TARGET_IP" "yes" \
  "$(contains "Error during parsing" \
    "$(output_of -e DNS_ZONE="$ZONE" -e TRAEFIK_API="http://$STUB:8080/api/http/routers")")"
check "malformed TARGET_IP" "yes" \
  "$(contains "is not an IP address" \
    "$(output_of -e DNS_ZONE="$ZONE" -e TRAEFIK_API="http://$STUB:8080/api/http/routers" -e TARGET_IP=not-an-ip)")"
check "malformed TRAEFIK_API" "yes" \
  "$(contains "http or https scheme" \
    "$(output_of -e DNS_ZONE="$ZONE" -e TRAEFIK_API="traefik:8080/routers" -e TARGET_IP="$TARGET")")"
check "missing DNS_ZONE" "yes" \
  "$(refuses_to_start -e TRAEFIK_API="http://$STUB:8080/api/http/routers" -e TARGET_IP="$TARGET")"
# Constraint C2: this must never become a catch-all resolver. Serving the root
# zone would put it in front of all resolution, and it never forwards.
check "root zone is refused" "yes" \
  "$(contains "refusing to serve the root zone" \
    "$(output_of -e DNS_ZONE="." -e TRAEFIK_API="http://$STUB:8080/api/http/routers" -e TARGET_IP="$TARGET")")"
# A non-numeric DNS_PORT makes CoreDNS read the whole ZONE:PORT string as a zone
# name and fall back to port 53. Left alone that starts a server listening on
# the wrong port, authoritative for a zone nobody will query.
check "non-numeric DNS_PORT" "yes" \
  "$(contains "contains a colon" \
    "$(output_of -e DNS_ZONE="$ZONE" -e DNS_PORT=notaport \
      -e TRAEFIK_API="http://$STUB:8080/api/http/routers" -e TARGET_IP="$TARGET")")"
check "out-of-range DNS_PORT" "yes" \
  "$(refuses_to_start -e DNS_ZONE="$ZONE" -e DNS_PORT=99999 \
    -e TRAEFIK_API="http://$STUB:8080/api/http/routers" -e TARGET_IP="$TARGET")"

echo
if [ "$failures" -ne 0 ]; then
  echo "$failures check(s) failed"
  echo "--- server log ---"
  docker logs "$DNS" 2>&1 | tail -30
  exit 1
fi
echo "All checks passed"
