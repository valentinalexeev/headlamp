# ssh-ca

This role configures sshd on a node to accept SSH certificates. With certificates, a CI job or an operator logs in with a short-lived certificate instead of a static key in `authorized_keys`. The role does these steps:

- It writes the public keys of the trusted user certificate authorities (CAs).
- It writes one principals file for each account. The file lists the certificate principals that can log in as that account.
- Optional: it signs the host keys with a host CA. Clients then trust all nodes through one `@cert-authority` line in `known_hosts`.
- It writes the sshd drop-in `sshd_config.d/40-ssh-ca.conf`, and it reloads sshd.

The role does not remove keys from `authorized_keys`. Keep one static key as the break-glass path.

## Supported systems

The role uses systemd and the OpenSSH layout of Debian and Ubuntu. `sshd_config` must include `sshd_config.d/*.conf`; the role stops if it does not. `make test-ssh-ca-e2e` tests the role with a real sshd on Ubuntu 26.04 and Debian 13.

## How it works

sshd reads the drop-in before the rest of `sshd_config`, so the drop-in settings apply first. The drop-in sets these items:

- `TrustedUserCAKeys` points to `ssh_ca_user_keys.pub`.
- `AuthorizedPrincipalsFile` points to `auth_principals/<account>`.
- `HostCertificate` is set once for each host certificate.

A certificate logs in as an account only when one of its principals is in the principals file of that account. An account without a principals file accepts no certificate. Keys in `authorized_keys` continue to work.

The role checks the new drop-in with `sshd -t` and the full configuration before it replaces the old drop-in. A configuration that sshd does not accept is not installed. The reload uses `systemctl try-reload-or-restart`. It does not start sshd when sshd does not run, for example with `ssh.socket`. Open sessions stay open.

## Use

There is no playbook in this repo. Apply the role from the private repo, after the base roles:

```yaml
- name: SSH certificates
  hosts: exit_nodes_vm
  roles:
    - role: ssh-ca
      tags: [ssh-ca]
```

Put the site values in `group_vars` of the private repo:

```yaml
ssh_ca_user_ca_keys:
  - "ssh-ed25519 AAAA... operator-ca"
  - "ssh-ed25519 AAAA... ci-ca"
ssh_ca_principals:
  - user: "{{ ansible_user }}"
    principals: [deploy]
ssh_ca_host_certs: true
ssh_ca_host_cert_names:
  - "{{ inventory_hostname }}"
  - "{{ ansible_host }}"
```

Use a different CA for each signer, for example one for the operator and one for CI. To stop one signer, remove its line from `ssh_ca_user_ca_keys` and apply the role.

## Variables

| Variable | Default | Use |
|---|---|---|
| `ssh_ca_user_ca_keys` | `[]` | The public keys of the trusted user CAs, one OpenSSH line each. Required. |
| `ssh_ca_principals` | `ansible_user` gets `deploy` | A list of `{user, principals}`. Principals cannot contain spaces or commas. |
| `ssh_ca_principals_exclusive` | `true` | Remove the principals files of accounts that are not in the list. |
| `ssh_ca_host_certs` | `false` | Sign the host keys and present the host certificates. |
| `ssh_ca_host_ca_key` | `""` | The path on the controller to the host CA private key, without a passphrase. |
| `ssh_ca_host_ca_pub` | `""` | The path on the controller to the host CA public key. The private key must be in the ssh-agent of the controller. |
| `ssh_ca_host_cert_names` | `inventory_hostname`, `ansible_host` | The host names in the certificate. A client compares the name that it connects to with this list. |
| `ssh_ca_host_cert_validity` | `+52w` | The validity of a new host certificate, in `ssh-keygen -V` format. |
| `ssh_ca_host_cert_renew_days` | `30` | Sign again when fewer days are left. |
| `ssh_ca_host_key_types` | `ed25519`, `ecdsa`, `rsa` | The host key types to sign. The role ignores a type that the host does not have. |
| `ssh_ca_sshd_dir` | `/etc/ssh` | The sshd configuration directory. |
| `ssh_ca_sshd_binary` | `/usr/sbin/sshd` | The sshd binary for `sshd -t`. |
| `ssh_ca_service` | `ssh` | The sshd service to reload. |
| `ssh_ca_manage_service` | `true` | Reload sshd after a change. |
| `ssh_ca_validate` | `true` | Check the configuration with `sshd -t` before the role installs it. |

## Host certificates

The host CA stays on the controller. The role copies a host public key to the controller, signs it there, and copies the certificate back to the host. The role signs a host key again only in these conditions:

- The certificate is missing.
- The host key changed, for example after a rebuild of the node.
- The host CA changed.
- The host names changed.
- The certificate expires in fewer than `ssh_ca_host_cert_renew_days` days.

When neither `ssh_ca_host_ca_key` nor `ssh_ca_host_ca_pub` is set, the role does not sign. It keeps the current certificate and shows a `WARNING` when a certificate must be signed. Thus a CI job that has no host CA can apply the role. Sign from the operator machine.

To keep the host CA private key in a password manager, load the key into the ssh-agent of that manager. Then set `ssh_ca_host_ca_pub` to the public key file.

Give clients one line in `known_hosts` for all nodes, with the name pattern of the nodes:

```text
@cert-authority *.<your-tailnet-domain> ssh-ed25519 AAAA... host-ca
```

## Certificates for CI

`scripts/ssh-cert-sign.sh` makes a new key and a short-lived user certificate for one CI job. The CI CA private key comes from the environment variable `SSH_CA_KEY`. The script loads the key into a private ssh-agent and stops the agent at the end. The script does not write the CA key to disk.

1. Make the CI CA on the operator machine: `ssh-keygen -t ed25519 -C ci-ca -f ci-ca`.
2. Add `ci-ca.pub` to `ssh_ca_user_ca_keys` and apply the role.
3. Store the private key `ci-ca` as a secret of the CI system, then delete the local file.
4. In the CI job, connect to the tailnet first.
5. In the CI job, make the certificate:

   ```sh
   scripts/ssh-cert-sign.sh --out "$RUNNER_TEMP/ssh" --id "gha-$GITHUB_RUN_ID" \
     --principals deploy --validity +30m \
     --source-address 100.64.0.0/10,fd7a:115c:a1e0::/48 \
     --host-ca-pub host-ca.pub --host-pattern '*.<your-tailnet-domain>'
   ```

6. Run Ansible with the key and the `known_hosts` file of the script:

   ```sh
   export ANSIBLE_PRIVATE_KEY_FILE="$RUNNER_TEMP/ssh/id_ed25519"
   export ANSIBLE_SSH_ARGS="-o UserKnownHostsFile=$RUNNER_TEMP/ssh/known_hosts -o StrictHostKeyChecking=yes -o IdentitiesOnly=yes"
   ```

**WARNING:** A job that can read the CI CA secret can sign certificates for all listed principals. Give the secret only to jobs on the protected branch, for example through a protected CI environment.

The certificate has these limits:

- It is valid from 5 minutes before the signature until the end of `--validity`. The maximum is 24 hours.
- It allows a terminal, but no agent, port or X11 forwarding.
- With `--source-address`, sshd accepts it only from these addresses. Use the tailnet address ranges.
- The key ID (`--id`) and the serial show in the sshd log of each node.

## Tests

- `ansible/roles/ssh-ca/tests/render.sh` applies the role to a temporary directory on the local machine. It checks the files, the signature of host keys, renewal and a second run. `make test-ansible-roles` runs it.
- `scripts/test/ssh-ca/run_tests.sh` tests `ssh-cert-sign.sh`. `make test-ssh-ca` runs it.
- `scripts/test/ssh-ca/e2e-docker.sh` applies the role to a container with a real sshd, and logs in with certificates. `make test-ssh-ca-e2e` runs it.
