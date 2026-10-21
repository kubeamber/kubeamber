#!/usr/bin/env bash
# End-to-end scenarios against a cluster running KubeAmber (see `make kind-demo`).
# An agent with cluster-admin -- the realistic worst case -- tries a series of
# changes. RBAC says yes to all of them; KubeAmber decides.
set -euo pipefail
cd "$(dirname "$0")/.."

CONTEXT=${CONTEXT:-kind-kubeamber}
KUBEAMBER=${KUBEAMBER:-kubeamber}
AGENT=system:serviceaccount:kubeamber-agents:sre-agent
CR_PATTERN='cr-[0-9a-f]{8}-[a-z0-9]{5}'

k() { kubectl --context "$CONTEXT" "$@"; }
agent() { kubectl --context "$CONTEXT" --as="$AGENT" "$@"; }
tp() { "$KUBEAMBER" --context "$CONTEXT" "$@"; }
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

pass=0
fail=0
LAST=""

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# check DESCRIPTION allowed|blocked REGEX -- COMMAND...
check() {
  local desc=$1 want=$2 pattern=$3
  shift 4
  local rc=0 got=allowed
  LAST=$("$@" 2>&1) || rc=$?
  [[ $rc -ne 0 ]] && got=blocked
  printf '%s\n' "$LAST" | fold -s -w 160 | sed 's/^/    | /'
  if [[ $got == "$want" ]] && grep -qE -- "$pattern" <<<"$LAST"; then
    printf '  \033[32mPASS\033[0m %s\n' "$desc"
    pass=$((pass + 1))
  else
    printf '  \033[31mFAIL\033[0m %s (wanted %s matching /%s/, got %s)\n' "$desc" "$want" "$pattern" "$got"
    fail=$((fail + 1))
  fi
}

# refute DESCRIPTION REGEX: the previous output must not match.
refute() {
  if grep -qE -- "$2" <<<"$LAST"; then
    printf '  \033[31mFAIL\033[0m %s\n' "$1"
    fail=$((fail + 1))
  else
    printf '  \033[32mPASS\033[0m %s\n' "$1"
    pass=$((pass + 1))
  fi
}

step "Setup: shop and prod namespaces, a checkout service in each, an agent with cluster-admin"
for ns in shop prod; do
  k create namespace "$ns" --dry-run=client -o yaml | k apply -f - >/dev/null
  k apply -n "$ns" -f examples/manifests/checkout.yaml >/dev/null
  k -n "$ns" scale deploy/checkout --replicas=3 >/dev/null
done
k -n kubeamber-agents create serviceaccount sre-agent --dry-run=client -o yaml | k apply -f - >/dev/null
k create clusterrolebinding sre-agent-cluster-admin --clusterrole=cluster-admin \
  --serviceaccount=kubeamber-agents:sre-agent --dry-run=client -o yaml | k apply -f - >/dev/null
k delete changerequests --all >/dev/null
k -n prod rollout status deploy/checkout --timeout=120s >/dev/null
for _ in $(seq 1 30); do
  expr=$(k get validatingwebhookconfiguration kubeamber -o jsonpath='{.webhooks[0].matchConditions[0].expression}')
  [[ $expr != "false" ]] && break
  sleep 2
done
echo "    agents webhook matches: $expr"

step "1. Humans are never touched"
check "a human restarts prod/checkout" allowed "restarted" -- k -n prod rollout restart deploy/checkout
refute "  ...and KubeAmber says nothing" "kubeamber"

step "2. Low-risk agent changes go straight through, with a note"
check "agent scales shop/checkout 3 -> 4" allowed "kubeamber: ALLOW \(risk 15/100" -- agent -n shop scale deploy/checkout --replicas=4
check "agent edits a ConfigMap in shop" allowed "kubeamber: ALLOW" -- \
  agent -n shop patch configmap checkout-config --type merge -p '{"data":{"LOG_LEVEL":"debug"}}'

step "3. Risky changes are held for a human"
check "agent opens a shell in a prod pod" blocked "kubeamber: HOLD .*interactive session \(exec\)" -- \
  agent -n prod exec deploy/checkout -c app -- true
check "a server-side dry run is judged but raises nothing" blocked "dry run, so no change request" -- \
  agent -n prod scale deploy/checkout --replicas=0 --dry-run=server
check "agent scales prod/checkout to 0" blocked "kubeamber: HOLD \(risk 75/100, replicas 3 → 0\)" -- \
  agent -n prod scale deploy/checkout --replicas=0
CR=$(grep -oE "$CR_PATTERN" <<<"$LAST" | head -1)
check "retrying before approval raises no duplicate" blocked "$CR is still waiting for a human" -- \
  agent -n prod scale deploy/checkout --replicas=0
check "kubeamber list shows it" allowed "$CR +Pending +sre-agent +UPDATE Deployment prod/checkout +75" -- tp list

step "4. Nobody can approve a change on someone else's behalf"
check "the agent approves its own change request" blocked "agents cannot" -- \
  agent patch changerequest "$CR" --type merge \
  -p "{\"spec\":{\"approval\":{\"decision\":\"Approved\",\"by\":\"$AGENT\",\"at\":\"$(now)\"}},\"status\":{\"phase\":\"Approved\"}}"
check "a human signs as someone else" blocked "must be your own identity" -- \
  k patch changerequest "$CR" --type merge \
  -p "{\"spec\":{\"approval\":{\"decision\":\"Approved\",\"by\":\"mallory\",\"at\":\"$(now)\"}},\"status\":{\"phase\":\"Approved\"}}"
check "a human tampers with the request" blocked "only spec.approval" -- \
  k patch changerequest "$CR" --type merge -p '{"spec":{"score":1}}'

step "5. A human approves; the identical retry goes through, once"
check "operator approves" allowed "approved $CR" -- tp approve "$CR" --reason "ok, investigating the crash loop"
check "agent retries and is allowed" allowed "kubeamber: ALLOW \(approved by .* in change request $CR\)" -- \
  agent -n prod scale deploy/checkout --replicas=0
check "prod/checkout is at 0 replicas" allowed "^0$" -- k -n prod get deploy checkout -o jsonpath='{.spec.replicas}'
check "the approval is used up" allowed "Consumed" -- k get changerequest "$CR"
k -n prod scale deploy/checkout --replicas=3 >/dev/null

step "6. Rejections stick"
check "agent deletes prod/checkout" blocked "kubeamber: HOLD \(risk 60/100, removes 3 pod" -- agent -n prod delete deploy checkout
CR2=$(grep -oE "$CR_PATTERN" <<<"$LAST" | head -1)
check "operator rejects" allowed "rejected $CR2" -- tp reject "$CR2" --reason "roll back the image instead"
check "the agent's retry is refused with the reason" blocked "roll back the image instead" -- agent -n prod delete deploy checkout

step "7. Dangerous changes are denied outright"
check "agent deletes namespace prod" blocked "kubeamber: DENY .*everything in it" -- agent delete namespace prod --dry-run=server
check "agent grants itself cluster-admin" blocked "may not change RBAC" -- \
  agent apply -f examples/manifests/escalate-privileges.yaml

step "8. Agents cannot switch KubeAmber off"
check "agent rewrites KubeAmber's policy" blocked "KubeAmber's namespace" -- \
  agent -n kubeamber-system patch configmap kubeamber-policy --type merge -p '{"data":{"policy.yaml":"{}"}}'
check "agent scales KubeAmber to zero" blocked "KubeAmber's namespace" -- agent -n kubeamber-system scale deploy/kubeamber --replicas=0
check "agent deletes the ChangeRequest CRD" blocked "KubeAmber's API definitions" -- \
  agent delete crd changerequests.kubeamber.dev --dry-run=server
check "agent deletes KubeAmber's ClusterRole" blocked "KubeAmber's permissions" -- agent delete clusterrole kubeamber --dry-run=server
check "agent deletes KubeAmber's webhook configuration" blocked "agents cannot change admission webhooks" -- \
  agent delete validatingwebhookconfiguration kubeamber --dry-run=server
# Known limit: the API server exempts admission policies and their bindings
# from all admission control, so nothing at admission can stop this. Only
# RBAC can (docs/threat-model.md). The test pins the behaviour so a change in
# Kubernetes is noticed.
check "KNOWN LIMIT: cluster-admin agent can delete the protecting policy binding" allowed "deleted \(server dry run\)" -- \
  agent delete validatingadmissionpolicybinding kubeamber-protect-admission --dry-run=server

step "9. Change management: the approval comes from ServiceNow (simulated)"
SNOW_ADDR=127.0.0.1:18080
SNOW=http://$SNOW_ADDR
WORK=$(mktemp -d)
printf 'kubeamber.integration' >"$WORK/username"
printf 'demo-only' >"$WORK/password"
cat >"$WORK/bridge.yaml" <<YAML
apiVersion: kubeamber.dev/v1alpha1
kind: BridgeConfig
spec:
  interval: 2s
  servicenow:
    instanceURL: $SNOW
    auth: {type: basic, usernameFile: $WORK/username, passwordFile: $WORK/password}
    configurationItems: {"prod*": ci-prod-cluster}
YAML
go build -o bin/fakesnow ./hack/fakesnow
bin/fakesnow --addr "$SNOW_ADDR" >"$WORK/fakesnow.log" 2>&1 &
SNOW_PID=$!
k apply -f deploy/servicenow-bridge/rbac.yaml >/dev/null
"$KUBEAMBER" bridge --context "$CONTEXT" --as system:serviceaccount:kubeamber-system:kubeamber-bridge \
  --config "$WORK/bridge.yaml" >"$WORK/bridge.log" 2>&1 &
BRIDGE_PID=$!
trap 'kill $SNOW_PID $BRIDGE_PID 2>/dev/null || true' EXIT

# wait_for SECONDS COMMAND...: retry until the command succeeds.
wait_for() {
  local secs=$1
  shift
  for _ in $(seq 1 "$secs"); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

check "agent cuts prod/checkout from 3 to 1" blocked "kubeamber: HOLD \(risk 55/100, replicas 3 → 1\)" -- \
  agent -n prod scale deploy/checkout --replicas=1
CR3=$(grep -oE "$CR_PATTERN" <<<"$LAST" | head -1)
wait_for 30 sh -c "kubectl --context $CONTEXT get changerequest $CR3 -o jsonpath='{.status.external.number}' | grep -q CHG"
check "the bridge opened a ServiceNow ticket" allowed "^CHG[0-9]{7}$" -- \
  k get changerequest "$CR3" -o jsonpath='{.status.external.number}'
TICKET=$LAST
check "the ticket carries the agent's reasoning and the CMDB item" allowed '"cmdb_ci":"ci-prod-cluster".*cuts replicas from 3 to 1|cuts replicas from 3 to 1.*"cmdb_ci":"ci-prod-cluster"' -- \
  curl -fsS "$SNOW/fake/changes/$TICKET"
check "the agent is pointed at the ticket" blocked "waiting for approval in servicenow as $TICKET" -- \
  agent -n prod scale deploy/checkout --replicas=1
check "kubeamber list shows the ticket" allowed "$CR3 +Pending .* $TICKET" -- tp list
check "a human cannot repoint the ticket" blocked "maintained by the change-management bridge" -- \
  k patch changerequest "$CR3" --type merge -p '{"status":{"external":{"number":"CHG0000666"}}}'
check "the CAB approves in ServiceNow" allowed '"approval":"approved"' -- \
  curl -fsS -X POST "$SNOW/fake/changes/$TICKET/approve?by=jane.cab"
wait_for 30 sh -c "kubectl --context $CONTEXT get changerequest $CR3 -o jsonpath='{.status.phase}' | grep -q Approved"
check "the bridge records the ServiceNow approver" allowed "^servicenow:jane.cab$" -- \
  k get changerequest "$CR3" -o jsonpath='{.spec.approval.by}'
check "the agent's retry is applied" allowed "approved by servicenow:jane.cab in servicenow $TICKET" -- \
  agent -n prod scale deploy/checkout --replicas=1
wait_for 30 sh -c "curl -fsS $SNOW/fake/changes/$TICKET | grep -q '\"close_code\":\"successful\"'"
check "the bridge closes the ticket with evidence" allowed '"close_code":"successful".*Applied by AI agent sre-agent|Applied by AI agent sre-agent.*"close_code":"successful"' -- \
  curl -fsS "$SNOW/fake/changes/$TICKET"
k -n prod scale deploy/checkout --replicas=3 >/dev/null

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[[ $fail -eq 0 ]]
