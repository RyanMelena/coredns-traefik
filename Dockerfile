# syntax=docker/dockerfile:1

# Stage 1 - compile CoreDNS with the traefik plugin linked in.
#
# External CoreDNS plugins are compiled in, not loaded at runtime: the plugin is
# registered by adding a line to plugin.cfg and rebuilding the binary.
#
# The stage runs on the build platform and cross-compiles to the target, so a
# multi-arch build costs one Go build per architecture rather than a QEMU
# emulated toolchain.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

# Bumping this is the standing maintenance obligation for this repo; the plugin
# tests are what tell you whether the plugin API moved under it.
ARG COREDNS_VERSION=v1.14.6

# The import path the plugin is registered under. Must match the module path in
# go.mod.
ARG PLUGIN_MODULE=github.com/RyanMelena/coredns-traefik

ARG TARGETOS
ARG TARGETARCH
ARG BUILDARCH

RUN apk add --no-cache git make

# Build and test the plugin's own module first, so a compile or test failure
# surfaces as itself rather than as a confusing CoreDNS link error later.
WORKDIR /plugin
COPY go.mod go.sum ./
RUN go mod download
COPY plugin ./plugin
RUN CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go test ./...

WORKDIR /src
RUN git clone --depth 1 -b ${COREDNS_VERSION} https://github.com/coredns/coredns.git .

# Position in plugin.cfg sets the plugin's position in the chain; the order of
# directives in the Corefile does not. Ahead of hosts puts it after every
# middleware plugin (log, errors, prometheus, health) and ahead of every other
# record source.
RUN sed -i "s#^hosts:hosts#traefik:${PLUGIN_MODULE}/plugin/traefik\nhosts:hosts#" plugin.cfg \
 && grep -qx "traefik:${PLUGIN_MODULE}/plugin/traefik" plugin.cfg \
 && go mod edit -require=${PLUGIN_MODULE}@v0.0.0 -replace=${PLUGIN_MODULE}=/plugin \
 && GOFLAGS="-buildvcs=false" make gen

# Fail the build rather than ship an image whose plugin silently did not link.
# These two files are what actually import and order the plugin, and checking
# them works for a cross-compiled target as well.
RUN grep -q "${PLUGIN_MODULE}/plugin/traefik" core/plugin/zplugin.go \
 && grep -q '"traefik"' core/dnsserver/zdirectives.go

RUN GOFLAGS="-buildvcs=false" make SYSTEM="GOOS=${TARGETOS} GOARCH=${TARGETARCH}"

# When not cross-compiling, confirm against the binary itself. -plugins prints
# one bare plugin name per line, so match the whole line.
RUN if [ "${TARGETARCH}" = "${BUILDARCH}" ]; then /src/coredns -plugins | grep -qx "traefik"; fi

# Stage 2 - runtime. Static distroless: no shell, no package manager, non-root
# by default (UID 65532).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /src/coredns /coredns
COPY Corefile /etc/coredns/Corefile

# Optional knobs get defaults here so an operator only has to supply the three
# values specific to their network. The required ones - DNS_ZONE, TRAEFIK_API,
# TARGET_IP - are deliberately absent: unset, they substitute to nothing and
# CoreDNS refuses to start rather than serving a wrong zone.
ENV DNS_PORT=5353 \
    METRICS_PORT=9153 \
    POLL_INTERVAL=30s \
    RECORD_TTL=60 \
    API_TIMEOUT=5s

# DNS_PORT defaults to 5353 so that binding works on any runtime regardless of
# net.ipv4.ip_unprivileged_port_start; map the host's 53 onto it. EXPOSE is
# static metadata and cannot follow the environment, so it records the
# defaults; on macvlan, where the ports actually matter, EXPOSE is irrelevant.
EXPOSE 5353/udp 5353/tcp 9153

ENTRYPOINT ["/coredns", "-conf", "/etc/coredns/Corefile"]
