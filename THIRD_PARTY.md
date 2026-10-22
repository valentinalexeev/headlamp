# Third-party code

- `apps/fly/exit-ams/` is derived from [patte/fly-tailscale-exit](https://github.com/patte/fly-tailscale-exit)
  (a Fly.io Tailscale exit-node setup), adapted for headscale and extended with a route
  advertiser. That upstream repository did not carry an explicit license file at the time of
  writing; if you are its author and want this attributed differently or removed, please open
  an issue.
- `apps/fly/exit-ams/install.sh` is the Tailscale install script (BSD-3-Clause, see its header).
- `apps/tailguard/` runs the container image from [juhovh/tailguard](https://github.com/juhovh/tailguard)
  (not vendored here; see that project for its license).
