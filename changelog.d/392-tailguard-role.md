---
issue: 392
type: feature
section: Added
status: done
breaking: false
title: Ansible role for tailguard
components: [ansible, tailguard]
---
## Summary

New Ansible role `ansible/roles/tailguard`: installs Docker Compose and the pinned tailguard image, writes
`docker-compose.yml`, `.env` and `wg0.conf`, makes the WireGuard server key on the host (Ansible never reads the
private key, the run prints the public key), logs the node in with the one-time key `tailguard_authkey` through a
0600 file that it removes, and checks the exit node and the egress. Peers are the variable `tailguard_peers`. The role
is off by default: it runs only on hosts in the `tailguard` group (`examples/private-repo/ansible`). The manual
procedure in `apps/tailguard/README.md` still works.
