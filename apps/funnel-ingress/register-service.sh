#!/bin/sh
# Publish (or withdraw) a service of THIS tailnet node through funnel-ingress.
#
#   register-service.sh [--scheme http|https] SERVICE PORT [NAMESPACE]
#   register-service.sh --delete SERVICE [NAMESPACE]
#
# The node must carry the funnel tag and a namespace tag (set by the pre-auth key or the ACL). The backend
# address is this node's own tailnet address; the script cannot name another one. The call is idempotent, so
# run it at boot or from a timer. NAMESPACE may be omitted when the node has exactly one namespace tag.
#
# Environment:
#   FUNNEL_API_HOST   tailnet name or IP of the funnel-ingress node (required)
#   FUNNEL_API_PORT   registration API port, default 9100
#
# Exits non-zero on any answer other than success. Needs curl.
set -eu

usage() {
	echo "usage: $0 [--scheme http|https] SERVICE PORT [NAMESPACE]" >&2
	echo "       $0 --delete SERVICE [NAMESPACE]" >&2
	exit 2
}

delete=0
scheme=""
while [ $# -gt 0 ]; do
	case "$1" in
	--delete) delete=1; shift ;;
	--scheme)
		[ $# -ge 2 ] || usage
		scheme=$2
		shift 2
		;;
	-h | --help) usage ;;
	--) shift; break ;;
	-*) usage ;;
	*) break ;;
	esac
done

: "${FUNNEL_API_HOST:?set FUNNEL_API_HOST to the tailnet name or IP of funnel-ingress}"
port_api=${FUNNEL_API_PORT:-9100}
base="http://${FUNNEL_API_HOST}:${port_api}"

if [ "$delete" -eq 1 ]; then
	[ $# -ge 1 ] && [ $# -le 2 ] || usage
	service=$1
	ns=${2:-}
else
	[ $# -ge 2 ] && [ $# -le 3 ] || usage
	service=$1
	port=$2
	ns=${3:-}
	case "$port" in
	'' | *[!0-9]*) echo "PORT must be a number" >&2; exit 2 ;;
	esac
fi
case "$service" in
'' | *[!a-z0-9-]*) echo "SERVICE must be a lower-case DNS label (a-z, 0-9, -)" >&2; exit 2 ;;
esac
case "$ns" in
*[!a-z0-9-]*) echo "NAMESPACE must be a lower-case DNS label (a-z, 0-9, -)" >&2; exit 2 ;;
esac
case "$scheme" in
'' | http | https) ;;
*) echo "--scheme must be http or https" >&2; exit 2 ;;
esac

if [ "$delete" -eq 1 ]; then
	query=""
	[ -n "$ns" ] && query="?ns=${ns}"
	code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 -X DELETE "${base}/v1/routes/${service}${query}")
else
	body="{\"port\":${port}"
	[ -n "$ns" ] && body="${body},\"ns\":\"${ns}\""
	[ -n "$scheme" ] && body="${body},\"scheme\":\"${scheme}\""
	body="${body}}"
	code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 15 -X PUT -H 'Content-Type: application/json' -d "$body" "${base}/v1/routes/${service}")
fi

case "$code" in
200 | 204) ;;
401) echo "funnel-ingress does not know this node (401)" >&2; exit 1 ;;
403) echo "refused (403): the node needs the funnel tag and a namespace tag, the port must be allowed for the namespace, and the name must not be blocked" >&2; exit 1 ;;
409) echo "conflict (409): the name belongs to another node or to a static route" >&2; exit 1 ;;
422) echo "rejected (422): bad name, port or namespace (a node with several namespace tags must pass NAMESPACE)" >&2; exit 1 ;;
*) echo "unexpected answer: HTTP ${code}" >&2; exit 1 ;;
esac
