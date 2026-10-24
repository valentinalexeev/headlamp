ARG TSVERSION=1.102.2
ARG TSFILE=tailscale_${TSVERSION}_amd64.tgz

FROM alpine:3.18 as tailscale
ARG TSFILE
WORKDIR /app

RUN wget https://pkgs.tailscale.com/stable/${TSFILE} && \
  tar xzf ${TSFILE} --strip-components=1
COPY . ./


FROM alpine:3.18
RUN apk update && apk add ca-certificates iptables ip6tables iproute2 ethtool bind-tools redis age && rm -rf /var/cache/apk/*

# creating directories for tailscale
RUN mkdir -p /var/run/tailscale
RUN mkdir -p /var/cache/tailscale
RUN mkdir -p /var/lib/tailscale

# Copy binary to production image
COPY --from=tailscale /app/tailscaled /app/tailscaled
COPY --from=tailscale /app/tailscale /app/tailscale
COPY --from=tailscale /app/start.sh /app/start.sh
COPY --from=tailscale /app/direct-hosts.txt /app/direct-hosts.txt
COPY --from=tailscale /app/update-direct-routes.sh /app/update-direct-routes.sh
COPY --from=tailscale /app/state-sync.sh /app/state-sync.sh

# Run on container startup.
USER root
CMD ["/app/start.sh"]
