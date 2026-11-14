# Throwaway systemd host for the Ansible OS matrix (#170). Not published.
ARG BASE=ubuntu:26.04
FROM ${BASE}
ARG EXTRA=""
ENV DEBIAN_FRONTEND=noninteractive container=docker
# Only what a cloud image has before Ansible runs: systemd, python3, sudo and the SSH server that Ansible logs in
# through (the ssh-ca role configures it). EXTRA adds the server package set.
# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends systemd systemd-sysv python3 sudo ca-certificates openssh-server ${EXTRA} \
 && rm -rf /var/lib/apt/lists/* \
 && systemctl mask systemd-firstboot.service getty.target console-getty.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
