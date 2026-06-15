#!/usr/bin/env bash
#
# probe.sh — actively reproduce watchdog check failures and pinpoint the phase.
#
# The watchdog reports hosts as "cannot be reached" / "context deadline
# exceeded" every few hours, yet the sites look fine from a browser. A browser
# is a different vantage point than the watchdog pod (different DNS resolver,
# network path, CPU budget). This script probes the same URLs the watchdog does,
# on a tight loop, recording per-phase timing (DNS / TCP connect / TLS / first
# byte) so that when a blip happens you can see *which* phase degraded.
#
# Run it BOTH inside the cluster (a debug pod in the watchdog's namespace) and
# on a workstation, at the same time:
#   - in-cluster blips but workstation stays clean -> cluster / DNS / pod
#   - both blip together                            -> real upstream blip
#
# --- Run in-cluster (same vantage as the watchdog) -------------------------
#   NS=<watchdog-namespace>
#   kubectl run watchdog-probe -n "$NS" --image=nicolaka/netshoot \
#     --restart=Never --command -- sleep infinity
#   kubectl cp scripts/probe.sh "$NS"/watchdog-probe:/tmp/probe.sh
#   kubectl exec -n "$NS" -it watchdog-probe -- bash /tmp/probe.sh
#   # ... let it run until a blip is captured, then:
#   kubectl delete pod watchdog-probe -n "$NS"
#
# --- Run on a workstation --------------------------------------------------
#   bash scripts/probe.sh
#
# Tunables (environment variables):
#   INTERVAL   seconds between rounds            (default 5)
#   MAX_TIME   per-request timeout, seconds      (default 5, matches watchdog)
#   METHOD     HTTP method                       (default GET)
#   COUNT      number of rounds, 0 = run forever (default 0)
#
# Pass URLs as arguments to override the default list.

set -u

INTERVAL="${INTERVAL:-5}"
MAX_TIME="${MAX_TIME:-5}"
METHOD="${METHOD:-GET}"
COUNT="${COUNT:-0}"

# Defaults: the hosts that have been flapping, plus their shared front-end.
DEFAULT_URLS=(
  "https://yivi.app"
  "https://open.yivi.app"
  "https://privacybydesign.foundation"
  "https://schemes.yivi.app"
  "https://keyshare.yivi.app"
)

if [ "$#" -gt 0 ]; then
  URLS=("$@")
else
  URLS=("${DEFAULT_URLS[@]}")
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "error: curl not found. Use a debug image such as nicolaka/netshoot." >&2
  exit 1
fi

# Pick whatever DNS tool is available; DNS is a prime suspect, so resolve the
# host explicitly (separate from curl) to catch intermittent resolver failures.
dns_tool=""
if command -v dig >/dev/null 2>&1; then
  dns_tool="dig"
elif command -v nslookup >/dev/null 2>&1; then
  dns_tool="nslookup"
elif command -v getent >/dev/null 2>&1; then
  dns_tool="getent"
fi

now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

host_of() {
  # strip scheme and path: https://open.yivi.app/foo -> open.yivi.app
  printf '%s' "$1" | sed -E 's#^[a-z]+://##; s#/.*$##'
}

# Map the common curl exit codes to a readable cause.
curl_cause() {
  case "$1" in
    0)  printf 'ok' ;;
    5)  printf 'PROXY-RESOLVE-FAIL' ;;
    6)  printf 'DNS-RESOLVE-FAIL' ;;
    7)  printf 'CONNECT-FAIL' ;;
    28) printf 'TIMEOUT' ;;
    35) printf 'TLS-HANDSHAKE-FAIL' ;;
    52) printf 'EMPTY-REPLY' ;;
    56) printf 'RECV-ERROR' ;;
    *)  printf 'ERR(exit=%s)' "$1" ;;
  esac
}

probe_dns() {
  # A resolver-only check, separate from curl, to catch intermittent DNS
  # failures (SERVFAIL/timeout). curl's namelookup field already times the DNS
  # of the HTTP path, so here we only report reachability of the resolver.
  local host="$1" rc
  [ -z "$dns_tool" ] && { printf 'dns=skip'; return; }
  case "$dns_tool" in
    dig)      dig +time=2 +tries=1 +short A "$host" >/dev/null 2>&1; rc=$? ;;
    nslookup) nslookup "$host"     >/dev/null 2>&1; rc=$? ;;
    getent)   getent hosts "$host" >/dev/null 2>&1; rc=$? ;;
  esac
  if [ "$rc" -eq 0 ]; then printf 'dns=ok'; else printf 'dns=FAIL'; fi
}

probe_url() {
  local url="$1" host out rc code
  host="$(host_of "$url")"

  # %{time_*} are seconds with millisecond precision; on redirects they reflect
  # the final hop. remote_ip shows which backend actually answered.
  out=$(curl -sS -o /dev/null -L -X "$METHOD" --max-time "$MAX_TIME" \
    -w '%{http_code} %{time_namelookup} %{time_connect} %{time_appconnect} %{time_starttransfer} %{time_total} %{remote_ip}' \
    "$url" 2>/dev/null)
  rc=$?

  local dnscheck
  dnscheck="$(probe_dns "$host")"

  if [ "$rc" -ne 0 ]; then
    printf '%s  %-38s FAIL %-18s %s\n' "$(now)" "$url" "$(curl_cause "$rc")" "$dnscheck"
    return
  fi

  # shellcheck disable=SC2086
  set -- $out
  code="$1"
  printf '%s  %-38s code=%s namelookup=%ss connect=%ss tls=%ss ttfb=%ss total=%ss ip=%s %s\n' \
    "$(now)" "$url" "$code" "$2" "$3" "$4" "$5" "$6" "$7" "$dnscheck"
}

echo "probing ${#URLS[@]} url(s) every ${INTERVAL}s (timeout ${MAX_TIME}s, method ${METHOD}); dns tool: ${dns_tool:-none}"
echo "ctrl-c to stop"

round=0
while :; do
  for url in "${URLS[@]}"; do
    probe_url "$url"
  done
  round=$((round + 1))
  if [ "$COUNT" -ne 0 ] && [ "$round" -ge "$COUNT" ]; then
    break
  fi
  sleep "$INTERVAL"
done
