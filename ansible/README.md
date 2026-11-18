# Ansible playbooks

This directory has the playbooks for the VM exit nodes. Run them from a private site repository with the site inventory.

## Rotate the auth key of the VM exit nodes

`playbooks/rotate-authkey.yml` runs `tailscale up` on each host in `exit_nodes_vm`, with a new auth key.

### What a second login changes

The e2e test `make test-relogin-e2e` (`tests/e2e/relogin`) shows this behavior. It uses the pinned versions in `HEADSCALE_VERSION` and `TAILSCALE_VERSION`. The node is tagged, and its key is a one-time key.

| Case | Result |
|---|---|
| `tailscale up --auth-key=...` on a node that is logged in, without `--force-reauth` | The command does not use the key. The node key, the node record and the login do not change. |
| `tailscale up --force-reauth --auth-key=file:...` | The node gets a new node key. headscale keeps the node record: the same ID, name, IP addresses and tags. |
| The re-login fails, for example because the key is expired | The command fails with the cause. The old node key and the old login stay in use. The node stays online, also after a restart. |
| An admin expires the node (`headscale nodes expire`) | The node is logged out at once. `tailscale up` without a key and a restart do not bring it back. A new one-time key brings it back with the same node record. |

### Results for the playbook

- The playbook does not use `--force-reauth`. Thus, on a node that is logged in, it does not change the node key.
- To change the node key, use `--force-reauth`. A failed attempt does not stop the node, so you can try again with a new key.
- To bring back an expired node, give it a new one-time key.
- A re-registration connects to headscale on port 443 with TLS. Make sure that headscale is available on HTTPS.
