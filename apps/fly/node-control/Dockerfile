ARG TSVERSION=1.98.10
ARG TSFILE=tailscale_${TSVERSION}_amd64.tgz

FROM alpine:3.18 as tailscale
ARG TSFILE
WORKDIR /app
RUN wget https://pkgs.tailscale.com/stable/${TSFILE} && \
  tar xzf ${TSFILE} --strip-components=1

FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
  ca-certificates iptables iproute2 \
  && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /var/run/tailscale /var/cache/tailscale /var/lib/tailscale
COPY --from=tailscale /app/tailscaled /app/tailscaled
COPY --from=tailscale /app/tailscale /app/tailscale

WORKDIR /srv
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY app ./app
COPY start.sh /app/start.sh
RUN chmod +x /app/start.sh

USER root
CMD ["/app/start.sh"]
