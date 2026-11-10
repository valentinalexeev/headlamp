#!/bin/sh
# #260: the public enrolment paths live in one file, ansible/roles/reality-ingress/files/enroll-paths.caddy. The
# compose example keeps an identical copy (caddy/enroll-paths.caddy), because a site copies that directory. Fail when
# the copies differ. node-control's TestEdgePathsMatchThePublicRoutes checks the file against the routes.
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
src="$root/ansible/roles/reality-ingress/files/enroll-paths.caddy"
copy="$root/examples/compose/control/caddy/enroll-paths.caddy"
if ! cmp -s "$src" "$copy"; then
  echo "ERROR: $copy differs from $src. Copy it: cp $src $copy" >&2
  exit 1
fi
echo "enroll paths: OK"
