# Build context: this directory (apps/fly/node-control-go).
#
# TSVERSION documents which tailscale.com module the image is built against and is checked against
# go.mod, so a version bump in one place cannot drift from the other. The tailscale client itself is
# compiled into the binary (tsnet); there is no tailscaled and no start script.
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
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/node-control ./cmd/node-control

# alpine rather than a bare image: `fly ssh console` needs a shell.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates
WORKDIR /srv
COPY --from=build /out/node-control /usr/local/bin/node-control
# app/ is empty in the public tree; the private overlay adds nodes.json (optional) and, for enrolment, enroll.json.
COPY app ./app
ENTRYPOINT ["/usr/local/bin/node-control"]
