# Reality client/server compatibility matrix

`reality_compat.py` answers this question on loopback.
The question is: "does client X with fingerprint Y still connect to Xray server version Z?" A control proves that the lab itself works. Use the script before you change the pinned Xray version
(`xray_version` of the `reality-ingress` role). Use it also before you recommend a client.

Background: newer Xray servers can reject a ClientHello that lacks the hybrid post-quantum key share. A client
whose TLS library does not send it fails with `reality verification failed`. Only a test shows whether a given client and server pair
works. This matrix is that test.

## Run
1. Download the binaries yourself into a scratch directory.
   Compare each checksum with the digest that the project publishes (GitHub release assets show one). Example for macOS arm64:
   `Xray-macos-arm64-v8a.zip` (XTLS/Xray-core), `sing-box-<v>-darwin-arm64.tar.gz` (SagerNet/sing-box),
   mihomo's `mihomo-darwin-arm64-<v>.gz` (MetaCubeX/mihomo).
2. `./reality_compat.py --xray-server ./old/xray --xray-server ./new/xray --singbox ./sing-box --mihomo ./mihomo --fingerprints chrome,firefox,safari`
3. Delete the binaries after the run.

Exit code 1 means that the control (xray client, first fingerprint, every server) failed.
Fix the lab before you read the table. The `--dest` site and `--target` must be reachable from this machine.
