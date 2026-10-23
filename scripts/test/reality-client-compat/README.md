# Reality client/server compatibility matrix

`reality_compat.py` answers "does client X with fingerprint Y still connect to Xray server version Z?" on
loopback, with a control that proves the lab itself works. Use it before bumping the pinned Xray version
(`xray_version` of the `reality-ingress` role) and before recommending a client.

Background: newer Xray servers can reject a ClientHello that lacks the hybrid post-quantum key share; a client
whose TLS library does not send it fails with `reality verification failed`. Whether a given client/server pair
works is an empirical question, hence this matrix.

## Run
1. Download the binaries yourself, into a scratch directory, and verify each checksum against the project's
   published digest (GitHub release assets show one). Example for macOS arm64:
   `Xray-macos-arm64-v8a.zip` (XTLS/Xray-core), `sing-box-<v>-darwin-arm64.tar.gz` (SagerNet/sing-box),
   mihomo's `mihomo-darwin-arm64-<v>.gz` (MetaCubeX/mihomo).
2. `./reality_compat.py --xray-server ./old/xray --xray-server ./new/xray --singbox ./sing-box --mihomo ./mihomo --fingerprints chrome,firefox,safari`
3. Delete the binaries afterwards.

Exit code 1 means the control (xray client, first fingerprint, every server) failed: fix the lab (the `--dest`
site and `--target` must be reachable from this machine) before reading the table.
