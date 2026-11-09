# Container image for the dabberz control plane.
#
# This cannot run the real fork VMs: Firecracker needs /dev/kvm and a golden
# rootfs on the host, neither of which exists inside a container, so
# deploy/dabberz.container.yaml selects the local driver and says so. What the
# local driver gives up is isolation, not capability -- it runs a project's
# services and the verification harness as processes in this container.
#
# That is why the image carries a browser. Verification drives a real one at a
# preview over its public URL, so without it a fork can never reach the merge
# gate. Build with --build-arg WITH_BROWSER=0 for a control plane that only
# needs to be looked at; the image is several hundred megabytes smaller and
# verification reports that it could not start a browser.

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
        -o /out/dabberzctl ./cmd/dabberzctl \
 && CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/dabberz-verify ./cmd/dabberz-verify

FROM alpine:3.22

# ca-certificates for outbound HTTPS, git because the merge reviewer shells out
# to it, tzdata so timestamps in the activity feed render in a real zone.
RUN apk add --no-cache ca-certificates git tzdata

# The browser the verification harness drives, and fonts for it: without fonts
# a page renders as boxes, which makes every screenshot useless as evidence.
# WebGL is not available here -- that needs chromium-swiftshader, which is
# larger again than chromium itself.
ARG WITH_BROWSER=1
RUN if [ "$WITH_BROWSER" = "1" ]; then apk add --no-cache chromium font-noto; fi

COPY --from=build /out/dabberzd /usr/local/bin/dabberzd
COPY --from=build /out/dabberzctl /usr/local/bin/dabberzctl
COPY --from=build /out/dabberz-verify /usr/local/bin/dabberz-verify
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
