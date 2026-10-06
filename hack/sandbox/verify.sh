#!/usr/bin/env bash
# Proves the confinement from inside, the way an attacker would test it.
# Every "must fail" line is something the E5 review was able to do from
# the laptop.
set -uo pipefail
KC="kubectl --context kind-pra-sandbox -n paved-agent"
POD=$($KC get pod -l app=sandbox -o jsonpath='{.items[0].metadata.name}')
sh() { $KC exec "$POD" -c shell -- bash -c "$1" 2>&1; }
pass=0; fail=0
check() { # name, expected (0 = command must succeed, 1 = must fail), command
  local name=$1 want=$2 cmd=$3 out rc
  out=$(sh "$cmd"); rc=$?
  if { [ "$want" = 0 ] && [ $rc = 0 ]; } || { [ "$want" = 1 ] && [ $rc != 0 ]; }; then
    echo "ok    $name"; pass=$((pass+1))
  else
    echo "FAIL  $name  (rc=$rc) ${out:0:160}"; fail=$((fail+1))
  fi
}
echo "shell container of $POD"
check "no docker socket"                 1 "test -S /var/run/docker.sock"
check "no docker binary"                 1 "command -v docker"
check "not root"                         1 "test \$(id -u) = 0"
check "root filesystem read-only"        1 "touch /usr/x"
check "no service-account token"         1 "test -e /var/run/secrets/kubernetes.io/serviceaccount/token"
check "no credentials in environment"    1 "env | grep -Ei 'token|key|secret|anthropic'"
check "cannot see the worker's processes" 1 "ls /proc | grep -qE '^[0-9]+$' && pgrep -f change-agent"
check "no internet (1.1.1.1:443)"        1 "timeout 4 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443'"
check "no DNS (api.anthropic.com)"       1 "timeout 4 getent hosts api.anthropic.com"
check "no cluster API"                   1 "timeout 4 bash -c 'exec 3<>/dev/tcp/10.96.0.1/443'"
check "no metadata service"              1 "timeout 4 bash -c 'exec 3<>/dev/tcp/169.254.169.254/80'"
PROXY_IP=$($KC get networkpolicy sandbox-one-address -o jsonpath='{.spec.egress[0].to[0].ipBlock.cidr}' | cut -d/ -f1)
check "the proxy's port is reachable"    0 "timeout 4 bash -c 'exec 3<>/dev/tcp/$PROXY_IP/8471'"
check "but refuses the shell (no token)" 1 "timeout 4 bash -c 'exec 3<>/dev/tcp/$PROXY_IP/8471; printf \"GET /v1/fleet HTTP/1.0\r\n\r\n\" >&3; head -1 <&3 | grep -q \" 200 \"'"
check "shell has the reviewer's tools"   0 "command -v jq && command -v grep && command -v find && command -v diff"
echo
echo "worker container: confinement check at start"
$KC logs "$POD" -c worker --tail=50 | grep -E "confinement|refusing" || echo "FAIL  worker did not report confinement"
echo
echo "$pass ok, $fail failed"
[ $fail = 0 ]
