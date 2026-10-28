# Build context: this directory (apps/fly/funnel-ingress).
#
# TSVERSION documents which tailscale.com module the image is built against and is checked against
# go.mod, so a version bump in one place cannot drift from the other. The tailscale client itself is
# compiled into the binary (tsnet); there is no tailscaled and no start script. No volume: the node's
# state lives in the shared Redis (see README "State").
ARG TSVERSION=1.98.10

FROM golang:1.26.5-alpine AS build
ARG TSVERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN grep -q "tailscale.com v${TSVERSION}$" go.mod \
  || { echo "go.mod does not pin tailscale.com v${TSVERSION}" >&2; exit 1; }
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/funnel-ingress ./cmd/funnel-ingress

# alpine rather than a bare image: `fly ssh console` needs a shell.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
  && adduser -D -u 10001 funnel
COPY --from=build /out/funnel-ingress /usr/local/bin/funnel-ingress
USER funnel
# File state (no REDIS_URL) needs a writable FUNNEL_STATE_DIR; mount a volume there and chown it to uid 10001.
ENTRYPOINT ["/usr/local/bin/funnel-ingress"]
