#!/usr/bin/env bash
# Brings up the sandbox: a kind cluster with Calico (so NetworkPolicy is
# enforced), the two images, the pod, and the policy that leaves it one
# address — the proxy on this laptop, reached through Docker's host
# gateway. Idempotent; re-run after code changes to roll the pod.
#
# Credentials never appear on screen. The proxy token is generated once
# into the Keychain (service paved-proxy-token) and copied into a Secret;
# proxy.yaml must reference it as token: keychain:paved-proxy-token.
set -euo pipefail
cd "$(dirname "$0")/../.."

CLUSTER=pra-sandbox
CALICO=${CALICO:-v3.30.3}
LOCK=${LOCK:-agents.lock.json}

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  # No --wait: without a CNI the node is not Ready until Calico is in.
  kind create cluster --config deploy/kind-sandbox.yaml
fi
KC="kubectl --context kind-$CLUSTER"
$KC apply -f "https://raw.githubusercontent.com/projectcalico/calico/$CALICO/manifests/calico.yaml" >/dev/null
echo "waiting for calico"
$KC -n kube-system rollout status ds/calico-node --timeout=180s
$KC wait --for=condition=Ready node --all --timeout=120s

echo "building images"
docker build -q -f deploy/Dockerfile.worker -t paved-agent/worker:dev . 
docker build -q -f deploy/Dockerfile.shell  -t paved-agent/shell:dev .
kind load docker-image --name $CLUSTER paved-agent/worker:dev paved-agent/shell:dev

# The proxy's address as the cluster sees it: Docker's gateway to the host.
PROXY_IP=$(docker exec $CLUSTER-control-plane getent hosts host.docker.internal | awk '{print $1}')
test -n "$PROXY_IP" || { echo "cannot resolve host.docker.internal from the node" >&2; exit 1; }
ENV_ID=$(python3 -c "import json;print(json.load(open('$LOCK'))['environment_id'])")

if ! security find-generic-password -a "$USER" -s paved-proxy-token -w >/dev/null 2>&1; then
  security add-generic-password -a "$USER" -s paved-proxy-token -w "$(openssl rand -hex 24)"
  echo "generated a proxy token into the Keychain (paved-proxy-token)"
fi

sed "s#PROXY_IP#$PROXY_IP#g" deploy/sandbox.yaml | $KC apply -f -
$KC -n paved-agent create configmap sandbox --from-literal=environment_id="$ENV_ID" --dry-run=client -o yaml | $KC apply -f -
security find-generic-password -a "$USER" -s paved-proxy-token -w \
  | tr -d '\n' \
  | $KC -n paved-agent create secret generic sandbox --from-file=proxy_token=/dev/stdin --dry-run=client -o yaml \
  | $KC apply -f -
$KC -n paved-agent rollout restart deployment/sandbox
$KC -n paved-agent rollout status deployment/sandbox --timeout=180s
echo
echo "sandbox is up; the pod's one address is $PROXY_IP:8471 (the proxy on this laptop)."
echo "verify:  hack/sandbox/verify.sh"
echo "logs:    kubectl --context kind-$CLUSTER -n paved-agent logs deploy/sandbox -c worker -f"
