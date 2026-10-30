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

# decision_events NAMESPACE REASON TEXT: KubeAmber's Events with that reason,
# once one of them mentions TEXT (Events are written asynchronously).
decision_events() {
  local out=""
  for _ in $(seq 1 20); do
    out=$(k -n "$1" get events --field-selector "reason=$2" \
      -o jsonpath='{range .items[*]}{.reason} {.involvedObject.kind}/{.involvedObject.name} {.message}{"\n"}{end}')
    grep -qF -- "$3" <<<"$out" && break
    sleep 1
  done
  printf '%s\n' "$out"
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
POD=$(k -n prod get pods -l app=checkout -o jsonpath='{.items[0].metadata.name}')
# Profiles are explicit: kubectl's default changed between releases, and
# newer defaults add SYS_PTRACE, which scores as host-level access.
check "agent adds a debug container to a prod pod" blocked "kubeamber: HOLD \(risk 70/100\).*ephemeral container" -- \
  agent -n prod debug "$POD" --image=registry.k8s.io/pause:3.10 -c debugger --profile=restricted
check "  ...one that can trace processes is denied" blocked "kubeamber: DENY .*CAP_SYS_PTRACE" -- \
  agent -n prod debug "$POD" --image=registry.k8s.io/pause:3.10 -c tracer --profile=general
check "agent proxies into a prod pod" blocked "kubeamber: HOLD \(risk 60/100\).*interactive session \(proxy\)" -- \
  agent get --raw "/api/v1/namespaces/prod/pods/$POD:80/proxy/"
CSRDIR=$(mktemp -d)
openssl req -new -newkey rsa:2048 -nodes -keyout "$CSRDIR/key.pem" -subj "/CN=mallory" -out "$CSRDIR/csr.pem" 2>/dev/null
for who in agent human; do
  cat >"$CSRDIR/$who.yaml" <<YAML
apiVersion: certificates.k8s.io/v1
kind: CertificateSigningRequest
metadata: {name: e2e-$who-csr}
spec:
  request: $(base64 <"$CSRDIR/csr.pem" | tr -d '\n')
  signerName: kubernetes.io/kube-apiserver-client
  usages: [client auth]
YAML
done
check "agent asks for a client certificate" blocked "kubeamber: HOLD \(risk 40/100\).*client certificate" -- \
  agent create -f "$CSRDIR/agent.yaml"
k create -f "$CSRDIR/human.yaml" >/dev/null
check "agent approves someone's certificate request" blocked "kubeamber: HOLD \(risk 65/100\).*approves certificate signing request e2e-human-csr" -- \
  agent certificate approve e2e-human-csr
k delete csr e2e-human-csr --ignore-not-found >/dev/null
check "a server-side dry run is judged but raises nothing" blocked "dry run, so no change request" -- \
  agent -n prod scale deploy/checkout --replicas=0 --dry-run=server
check "agent scales prod/checkout to 0" blocked "kubeamber: HOLD \(risk 75/100, replicas 3 → 0\)" -- \
  agent -n prod scale deploy/checkout --replicas=0
CR=$(grep -oE "$CR_PATTERN" <<<"$LAST" | head -1)
check "retrying before approval raises no duplicate" blocked "$CR is still waiting for a human" -- \
  agent -n prod scale deploy/checkout --replicas=0
check "kubeamber list shows it" allowed "$CR +Pending +sre-agent +UPDATE Deployment prod/checkout +75" -- tp list
check "the hold is an Event on prod/checkout, with the approve command" allowed \
  "AgentChangeHeld Deployment/checkout .*kubeamber approve $CR" -- decision_events prod AgentChangeHeld "$CR"

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
check "  ...and the approved retry is an Event too" allowed "AgentChangeApproved Deployment/checkout .*$CR was approved" -- \
  decision_events prod AgentChangeApproved "$CR"
k -n prod scale deploy/checkout --replicas=3 >/dev/null

step "6. Rejections stick"
check "agent deletes prod/checkout" blocked "kubeamber: HOLD \(risk 60/100, removes 3 pod" -- agent -n prod delete deploy checkout
CR2=$(grep -oE "$CR_PATTERN" <<<"$LAST" | head -1)
check "operator rejects" allowed "rejected $CR2" -- tp reject "$CR2" --reason "roll back the image instead"
check "the agent's retry is refused with the reason" blocked "roll back the image instead" -- agent -n prod delete deploy checkout
check "  ...and the refusal is an Event" allowed "AgentChangeRejected Deployment/checkout .*rejected change request $CR2" -- \
  decision_events prod AgentChangeRejected "$CR2"

step "7. Dangerous changes are denied outright"
check "agent deletes namespace prod" blocked "kubeamber: DENY .*everything in it" -- agent delete namespace prod --dry-run=server
check "agent grants itself cluster-admin" blocked "may not change RBAC" -- \
  agent apply -f examples/manifests/escalate-privileges.yaml
check "  ...the denial of a cluster-scoped object is an Event in default" allowed \
  "AgentChangeDenied ClusterRoleBinding/sre-agent-is-admin .*may not change RBAC" -- \
  decision_events default AgentChangeDenied "sre-agent-is-admin"
NODE=$(k get nodes -o jsonpath='{.items[0].metadata.name}')
check "agent reaches a node's kubelet through the API server" blocked "kubeamber: DENY \(risk 85/100\).*kubelet" -- \
  agent get --raw "/api/v1/nodes/$NODE/proxy/healthz"

step "8. Agents cannot switch KubeAmber off"
check "agent rewrites KubeAmber's policy" blocked "KubeAmber's namespace" -- \
  agent -n kubeamber-system patch configmap kubeamber-policy --type merge -p '{"data":{"policy.yaml":"{}"}}'
check "agent scales KubeAmber to zero" blocked "KubeAmber's namespace" -- agent -n kubeamber-system scale deploy/kubeamber --replicas=0
check "agent deletes the ChangeRequest CRD" blocked "KubeAmber's API definitions" -- \
  agent delete crd changerequests.kubeamber.dev --dry-run=server
check "agent repoints KubeAmber's API at its own server" blocked "which server answers KubeAmber's API" -- \
  agent patch apiservice v1alpha1.kubeamber.dev --dry-run=server --type merge \
  -p '{"spec":{"service":{"namespace":"shop","name":"checkout","port":443},"insecureSkipTLSVerify":true}}'
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

step "10. A developer gives their agent its own short-lived identity"
k create clusterrolebinding e2e-bob-approver --clusterrole=kubeamber-approver --user=bob --dry-run=client -o yaml | k apply -f - >/dev/null
check "mint a kubeconfig for Claude Code" allowed "reviewed as agent profile" -- \
  tp agent-kubeconfig claude-code --role edit --namespace shop --namespace prod --duration 1h --output "$WORK/claude.kubeconfig"
claude_kubectl() { kubectl --kubeconfig "$WORK/claude.kubeconfig" "$@"; }
check "the agent runs as its own identity, not the developer's" allowed "kubeamber-agents:claude-code-" -- claude_kubectl auth whoami
check "its routine changes are reviewed as an agent's" allowed "kubeamber: ALLOW" -- claude_kubectl -n shop scale deploy/checkout --replicas=5
check "it only has the access it was given" blocked "[Ff]orbidden" -- claude_kubectl -n kube-system get pods
check "agents cannot mint a token for KubeAmber's bridge" blocked "KubeAmber's namespace" -- \
  agent -n kubeamber-system create token kubeamber-bridge
check "agents cannot quietly become another identity" blocked "HOLD .*mints a credential for service account default" -- \
  agent -n shop create token default --duration 10m
check "agents cannot hijack a developer's agent identity" blocked "agent identities owned by a person" -- \
  agent -n kubeamber-agents annotate serviceaccount "$(claude_kubectl auth whoami -o jsonpath='{.status.userInfo.username}' | cut -d: -f4)" \
  kubeamber.dev/owner=mallory --overwrite
check "a risky change by the agent is held" blocked "kubeamber: HOLD \(risk 75/100" -- claude_kubectl -n prod scale deploy/checkout --replicas=0
CR4=$(grep -oE "$CR_PATTERN" <<<"$LAST" | head -1)
check "the change request records who launched the agent" allowed "^kubernetes-admin$" -- \
  k get changerequest "$CR4" -o jsonpath='{.spec.owner}'
check "the developer cannot approve their own agent's change" blocked "you launched this agent" -- tp approve "$CR4"
check "a colleague can" allowed "approved $CR4" -- tp --as bob approve "$CR4" --reason "pairing on the incident"
check "the agent's retry is applied" allowed "approved by bob" -- claude_kubectl -n prod scale deploy/checkout --replicas=0
k -n prod scale deploy/checkout --replicas=3 >/dev/null
check "revoking the identity removes its access at once" allowed "access is removed now" -- tp agent-kubeconfig claude-code --revoke
check "  ...so the agent can do nothing" blocked "[Ff]orbidden|Unauthorized" -- claude_kubectl -n shop get pods
wait_for 30 sh -c "kubectl --kubeconfig $WORK/claude.kubeconfig -n shop get pods 2>&1 | grep -q Unauthorized"
check "  ...and within seconds its token is rejected outright" blocked "Unauthorized" -- claude_kubectl -n shop get pods

step "11. Prometheus can scrape KubeAmber's decisions, latency and approvals"
# Each replica counts the requests it reviewed, so read every replica (one that
# is shutting down may not answer).
scrape_all() {
  local pod
  for pod in $(k -n kubeamber-system get pods -l app.kubernetes.io/name=kubeamber -o name); do
    k get --raw "/api/v1/namespaces/kubeamber-system/pods/${pod#pod/}:9090/proxy/metrics" || true
  done
}
check "decisions are counted by agent and outcome" allowed \
  'kubeamber_decisions_total\{[^}]*gen_ai_agent_name="sre-agent"[^}]*kubeamber_outcome="hold"' -- scrape_all
check "  ...denials too" allowed 'kubeamber_decisions_total\{[^}]*kubeamber_outcome="deny"' -- scrape_all
check "risk scores are recorded as a histogram" allowed 'kubeamber_risk_score_bucket\{[^}]*le="30"' -- scrape_all
check "review latency is recorded by outcome" allowed \
  'kubeamber_review_duration_seconds_bucket\{[^}]*kubeamber_outcome="hold"[^}]*le="5"\} [1-9]' -- scrape_all
check "the wait for a decision is recorded, for approvals here" allowed \
  'kubeamber_approval_wait_seconds_count\{[^}]*kubeamber_decision="approved"[^}]*kubeamber_decision_source="cluster"[^}]*\} [1-9]' -- scrape_all
check "  ...rejections" allowed \
  'kubeamber_approval_wait_seconds_count\{[^}]*kubeamber_decision="rejected"[^}]*\} [1-9]' -- scrape_all
check "  ...and approvals in ServiceNow" allowed \
  'kubeamber_approval_wait_seconds_count\{[^}]*kubeamber_decision_source="servicenow"[^}]*\} [1-9]' -- scrape_all
check "change requests are counted by phase" allowed 'kubeamber_change_requests\{[^}]*kubeamber_phase="Pending"[^}]*\} [1-9]' -- scrape_all
check "  ...used ones" allowed 'kubeamber_change_requests\{[^}]*kubeamber_phase="Consumed"[^}]*\} [1-9]' -- scrape_all
check "  ...and rejected ones" allowed 'kubeamber_change_requests\{[^}]*kubeamber_phase="Rejected"[^}]*\} [1-9]' -- scrape_all
check "each replica reports its version" allowed 'kubeamber_build_info\{goversion="go[0-9.]+",version="[^"]+"\} 1' -- scrape_all

step "12. People hear about held changes in Slack, Teams or a webhook (simulated)"
HOOK_ADDR=${HOOK_ADDR:-127.0.0.1:18081}
printf 'http://%s/slack' "$HOOK_ADDR" >"$WORK/slack-url"
printf 'http://%s/teams' "$HOOK_ADDR" >"$WORK/teams-url"
printf 'http://%s/hook' "$HOOK_ADDR" >"$WORK/hook-url"
printf 'e2e-signing-key' >"$WORK/hook-key"
k -n kubeamber-system get configmap kubeamber-policy -o jsonpath='{.data.policy\.yaml}' >"$WORK/policy.yaml"
cat >"$WORK/notify.yaml" <<YAML
apiVersion: kubeamber.dev/v1alpha1
kind: NotifyConfig
spec:
  cluster: kind-e2e
  interval: 1s
  remindAfter: 0s
  channels:
    - {name: platform, slack: {urlFile: $WORK/slack-url}}
    - {name: oncall, teams: {urlFile: $WORK/teams-url}}
    - {name: audit, webhook: {urlFile: $WORK/hook-url, secretFile: $WORK/hook-key}}
  routes:
    - channels: [platform, audit]
    - channels: [oncall]
      events: [held]
      namespaces: [prod]
YAML
go build -o bin/fakehook ./hack/fakehook
bin/fakehook --addr "$HOOK_ADDR" --secret e2e-signing-key >"$WORK/fakehook.log" 2>&1 &
HOOK_PID=$!
k apply -f deploy/notify/rbac.yaml >/dev/null
"$KUBEAMBER" notify --context "$CONTEXT" --as system:serviceaccount:kubeamber-system:kubeamber-notify \
  --config "$WORK/notify.yaml" --policy "$WORK/policy.yaml" --namespace kubeamber-system >"$WORK/notify.log" 2>&1 &
NOTIFY_PID=$!
trap 'kill $SNOW_PID $BRIDGE_PID $HOOK_PID $NOTIFY_PID 2>/dev/null || true' EXIT

# received PATH TEXT: what the fake receiver got on PATH, once some of it mentions TEXT.
received() {
  wait_for 30 sh -c "curl -sf 'http://$HOOK_ADDR/received?path=$1' | grep -qF -- '$2'" || true
  curl -sf "http://$HOOK_ADDR/received?path=$1"
}

check "agent scales prod/checkout from 3 to 2" blocked "kubeamber: HOLD \(risk 40/100, replicas 3 → 2\)" -- \
  agent -n prod scale deploy/checkout --replicas=2
CR5=$(grep -oE "$CR_PATTERN" <<<"$LAST" | head -1)
check "Slack is told it needs approval, with the command" allowed \
  "\[kind-e2e\] Approval needed: UPDATE Deployment prod/checkout by agent sre-agent \(risk 40/100\).*kubeamber approve $CR5" -- \
  received /slack "$CR5"
check "on-call's Teams channel gets an Adaptive Card" allowed "kubeamber approve $CR5.*\"type\":\"AdaptiveCard\"" -- received /teams "$CR5"
check "the webhook gets signed JSON" allowed "\"event\":\"held\",\"signature\":\"valid\".*\"changeRequest\":\"$CR5\"" -- \
  received /hook "$CR5"
check "operator approves" allowed "approved $CR5" -- tp approve "$CR5" --reason "planned"
check "  ...and Slack hears who approved it" allowed "Approved: UPDATE Deployment prod/checkout by agent sre-agent.*By: kubernetes-admin" -- \
  received /slack "Approved: UPDATE"
check "the agent's retry is applied" allowed "kubeamber: ALLOW \(approved by" -- agent -n prod scale deploy/checkout --replicas=2
check "  ...and Slack hears it was applied" allowed "Applied: UPDATE Deployment prod/checkout by agent sre-agent" -- \
  received /slack "Applied: UPDATE"
check "on-call only got the hold, as routed" allowed '^1$' -- sh -c "curl -sf 'http://$HOOK_ADDR/received?path=/teams' | grep -c '$CR5'"
k -n prod scale deploy/checkout --replicas=3 >/dev/null

step "13. The serving certificate is renewed without a restart"
# Deleting the Secret forces a new certificate and CA, as an expiry would.
# restarts: each webhook pod with its restart count, so a replaced pod shows too.
restarts() {
  k -n kubeamber-system get pods -l app.kubernetes.io/name=kubeamber \
    -o jsonpath='{range .items[*]}{.metadata.name}={.status.containerStatuses[0].restartCount}{" "}{end}'
}
RESTARTS=$(restarts)
OLD_CA=$(k -n kubeamber-system get secret kubeamber-webhook-tls -o jsonpath='{.data.ca\.crt}')
k -n kubeamber-system delete secret kubeamber-webhook-tls >/dev/null
# ca_published: a replica has written a new Secret, and every webhook in the
# configuration trusts its CA.
ca_published() {
  local secret bundle
  secret=$(k -n kubeamber-system get secret kubeamber-webhook-tls -o jsonpath='{.data.ca\.crt}') || return 1
  [[ -n $secret && $secret != "$OLD_CA" ]] || return 1
  for bundle in $(k get validatingwebhookconfiguration kubeamber -o jsonpath='{.webhooks[*].clientConfig.caBundle}'); do
    [[ $bundle == "$secret" ]] || return 1
  done
}
# agents_served: ten agent changes in a row are reviewed, so both replicas
# serve a certificate the new CA verifies.
agents_served() {
  local i out
  for i in $(seq 1 10); do
    out=$(agent -n shop annotate configmap checkout-config --overwrite "e2e.kubeamber.dev/renewal=$i" 2>&1) || return 1
    grep -q "kubeamber: ALLOW" <<<"$out" || return 1
  done
}
renewed() { wait_for 90 ca_published && echo "new CA published"; }
served() { wait_for 60 agents_served && echo "10 agent changes allowed in a row"; }
check "a replica writes a new certificate and every webhook trusts its CA" allowed "published" -- renewed
check "  ...both replicas serve the new certificate to agents" allowed "allowed in a row" -- served
check "  ...without restarting or replacing either pod" allowed "^${RESTARTS}\$" -- restarts

printf '\n\033[1m%d passed, %d failed\033[0m\n' "$pass" "$fail"
[[ $fail -eq 0 ]]
