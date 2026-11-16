#!/usr/bin/env bash
# k8shealth one-command install: Kyverno + agent injection policy + manager (with bundled MySQL by default).
#
#   ./install.sh
#
# Needs: kubectl and helm pointing at the target cluster. Everything is idempotent, re-run to upgrade.
#
# Settings (environment variables, all optional):
#   K8SH_NAMESPACE        namespace for the manager                          (default k8shealth)
#   K8SH_MANAGER_IMAGE    manager image repository                           (default ghcr.io/k8shealth/health-manager)
#   K8SH_MANAGER_TAG      manager image tag                                  (default: the chart's)
#   K8SH_AGENT_IMAGE      full agent image reference                         (default ghcr.io/k8shealth/health-agent:1.0.314-rc.1)
#   K8SH_DOMAIN           label prefix for onboarding                        (default k8shealth.io)
#   K8SH_DB_URL           external MySQL JDBC url; when set the bundled MySQL is not deployed
#   K8SH_DB_USER / K8SH_DB_PASSWORD   credentials for the external MySQL
#   K8SH_MANAGER_CHART    path to the manager chart                          (default ../health-manager/helm-chart)
#   K8SH_HELM_ARGS        extra arguments appended to the manager's helm install
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NS="${K8SH_NAMESPACE:-k8shealth}"
DOMAIN="${K8SH_DOMAIN:-k8shealth.io}"
AGENT_IMAGE="${K8SH_AGENT_IMAGE:-ghcr.io/k8shealth/health-agent:1.0.314-rc.1}"
MANAGER_IMAGE="${K8SH_MANAGER_IMAGE:-ghcr.io/k8shealth/health-manager}"
MANAGER_CHART="${K8SH_MANAGER_CHART:-$HERE/charts/health-manager}"
MANAGER_URL="http://health-manager.${NS}.svc:8080"   # headless service: clients hit the pod directly, so use the container port

say() { printf '\n==> %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

command -v kubectl >/dev/null || die 'kubectl not found'
command -v helm    >/dev/null || die 'helm not found (https://helm.sh/docs/intro/install/)'
kubectl version --request-timeout=10s >/dev/null 2>&1 || die 'cannot reach the cluster with kubectl'
[[ -d $MANAGER_CHART ]] || die "manager chart not found: $MANAGER_CHART (set K8SH_MANAGER_CHART)"

KYVERNO_SUBCHART="$(ls "$HERE"/charts/k8shealth-policy/charts/kyverno-*.tgz | head -n1)"
[[ -n $KYVERNO_SUBCHART ]] || die 'Kyverno archive missing in charts/k8shealth-policy/charts'

# Pin the hook image to the engine version (the chart default is the floating tag 'latest').
cat > /tmp/k8sh-kyverno-base.yaml <<'EOF'
features:
  forceFailurePolicyIgnore:
    enabled: true
webhooksCleanup:
  image: {tag: 'v1.19.2'}
EOF
cat > /tmp/k8sh-kyverno-policy.yaml <<EOF
healthAgentSidecar:
  image: ${AGENT_IMAGE}
  domain: ${DOMAIN}
  managerUrl: ${MANAGER_URL}
  scriptNamespace: ${NS}
kyverno:
  features:
    forceFailurePolicyIgnore:
      enabled: true
  webhooksCleanup:
    image: {tag: 'v1.19.2'}
EOF

# Kyverno is only installed when the cluster does not have it yet; one we installed earlier is upgraded,
# anyone else's is reused as is.
if kubectl get crd clusterpolicies.kyverno.io >/dev/null 2>&1 && ! helm status kyverno -n kyverno >/dev/null 2>&1; then
  KYVERNO_EXISTING=1
  say 'Kyverno found on this cluster, reusing it'
else
  KYVERNO_EXISTING=0
  say 'Kyverno (engine and CRDs)'
  helm upgrade --install kyverno "$KYVERNO_SUBCHART" -n kyverno --create-namespace -f /tmp/k8sh-kyverno-base.yaml --wait --timeout 10m
fi

say 'Agent script (copied into each workload namespace on demand)'
helm upgrade --install health-agent "$HERE/charts/k8shealth-agent-script" -n "$NS" --create-namespace --set "namespace=$NS"

say "Agent injection policy (label ${DOMAIN}/enabled=enabled)"
if [[ $KYVERNO_EXISTING == 1 ]]; then
  helm upgrade --install k8shealth-policy "$HERE/charts/k8shealth-policy" -n "$NS" -f /tmp/k8sh-kyverno-policy.yaml --set kyverno.install=false --wait --timeout 10m
else
  # Helm cannot create the policy in the same release that installs Kyverno's CRDs, hence the second step.
  helm upgrade --install kyverno "$HERE/charts/k8shealth-policy" -n kyverno -f /tmp/k8sh-kyverno-policy.yaml --wait --timeout 10m
fi
say 'Manager'
ARGS=(--set "image.repository=$MANAGER_IMAGE" --set "agent.domain=$DOMAIN" --set selfMonitor.enabled=true --set onboarding.enabled=true --set cluster.inCluster.enabled=true)
[[ -n ${K8SH_MANAGER_TAG:-} ]] && ARGS+=(--set "image.tag=$K8SH_MANAGER_TAG")
ARGS+=(--set "selfMonitor.image.repository=${AGENT_IMAGE%:*}" --set "selfMonitor.image.tag=${AGENT_IMAGE##*:}")
if [[ -n ${K8SH_DB_URL:-} ]]; then
  ARGS+=(--set "externalDatabase.url=$K8SH_DB_URL" --set "externalDatabase.username=${K8SH_DB_USER:-health}" --set "externalDatabase.password=${K8SH_DB_PASSWORD:?set K8SH_DB_PASSWORD}")
else
  ARGS+=(--set mysql.enabled=true)
fi
# shellcheck disable=SC2086
helm upgrade --install k8shealth "$MANAGER_CHART" -n "$NS" --create-namespace "${ARGS[@]}" ${K8SH_HELM_ARGS:-} --wait --timeout 15m

say 'Done'
cat <<EOF
Open it:   kubectl -n $NS port-forward $(kubectl -n $NS get pod -l app.kubernetes.io/component=manager -o name | head -1) 8080:8080     (then http://localhost:8080)
Login:     admin
Password:  kubectl -n $NS get secret health-manager-admin -o jsonpath='{.data.password}' | base64 -d; echo
EOF
