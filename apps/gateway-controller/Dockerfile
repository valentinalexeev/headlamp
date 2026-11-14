# Build context: this directory (apps/gateway-controller). Build with:
#   docker build -t gateway-controller .
#
# The controller is one static binary. It needs no shell and no state. CI publishes it for linux/amd64 and
# linux/arm64: the Go build cross-compiles on the build platform.
FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w" \
  -o /out/gateway-controller ./cmd/gateway-controller

FROM alpine:3.24
RUN adduser -D -u 10001 controller
COPY --from=build /out/gateway-controller /usr/local/bin/gateway-controller
USER 10001
ENTRYPOINT ["/usr/local/bin/gateway-controller"]
