# Container image for the dabberz control plane.
#
# This is the *control plane only*. It deliberately cannot run the real fork
# VMs: Firecracker needs /dev/kvm and a golden rootfs on the host, neither of
# which exists inside a container. The image exists so the UI, the audit trail
# and the API can be looked at from anywhere while the rest is still being
# built. deploy/dabberz.container.yaml selects the degraded driver and says so.

FROM golang:1.25-alpine AS build

# git is needed by `go build` for VCS stamping and by nothing else at build time.
RUN apk add --no-cache git

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Pure Go all the way down: modernc.org/sqlite needs no cgo, so the binary is
# static and the runtime image needs no libc beyond what alpine ships.
ARG VERSION=container
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/dabberzd ./cmd/dabberzd \
 && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/dabberzctl ./cmd/dabberzctl

FROM alpine:3.22

# ca-certificates for outbound HTTPS, git because the merge reviewer shells out
# to it, tzdata so timestamps in the activity feed render in a real zone.
RUN apk add --no-cache ca-certificates git tzdata

COPY --from=build /out/dabberzd /usr/local/bin/dabberzd
COPY --from=build /out/dabberzctl /usr/local/bin/dabberzctl
COPY deploy/dabberz.container.yaml /etc/dabberz/dabberz.yaml

# The database, the per-repo memory store and the local driver's workspaces all
# live here. Mount a volume on it or a redeploy starts from an empty database.
RUN mkdir -p /var/lib/dabberz
VOLUME /var/lib/dabberz

EXPOSE 8080

# No HEALTHCHECK: the orchestrating platform decides how to probe, and a
# baked-in interval fights whatever it configures.

ENTRYPOINT ["/usr/local/bin/dabberzd"]
CMD ["--config", "/etc/dabberz/dabberz.yaml"]
