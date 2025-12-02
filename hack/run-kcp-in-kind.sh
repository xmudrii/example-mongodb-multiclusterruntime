#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "$0")/.."

echodate() {
  # do not use -Is to keep this compatible with macOS
  echo "[$(date +%Y-%m-%dT%H:%M:%S%:z)]" "$@"
}

kind="${KIND:-kind}"

KCP_CHART_VERSION="${KCP_CHART_VERSION:-0.16.0}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.20.2}"

CLUSTER_NAME="${CLUSTER_NAME:-kcp}"
KIND_KUBECONFIG="${KIND_KUBECONFIG:-kind.kubeconfig}"
KCP_ADMIN_KUBECONFIG="${KCP_ADMIN_KUBECONFIG:-kcp-admin.kubeconfig}"
KCP_CONTROLLER_KUBECONFIG="${KCP_CONTROLLER_KUBECONFIG:-kcp-controller.kubeconfig}"
kcp_hostname="${KCP_HOSTNAME:-kcp.dev.test}"

# Whether the script should update /etc/hosts automatically. Set to "true" to allow
# the script to append an entry; default is "false".
UPDATE_HOSTS="${UPDATE_HOSTS:-false}"

if ! command -v "$kind" >/dev/null 2>&1; then
  echodate "❌ $kind is not installed or not in PATH" >&2
  echodate $'\tInstall kind: https://kind.sigs.k8s.io/\n\tOr set the KIND environment variable to the path to the kind binary.' >&2
  exit 1
fi

echodate "🔍 Checking for kind cluster '${CLUSTER_NAME}'..."
if ! $kind get clusters | grep -w -q "$CLUSTER_NAME"; then
  echodate "⚙️ Creating kind cluster '${CLUSTER_NAME}'..."
  $kind create cluster \
    --name "$CLUSTER_NAME" \
    --config hack/kind-kcp/kind-config.yaml
  echodate "✅ Created kind cluster '${CLUSTER_NAME}'."
else
  echodate "✅ Cluster $CLUSTER_NAME already exists."
fi

$kind get kubeconfig --name "$CLUSTER_NAME" > "$KIND_KUBECONFIG"

echodate "📥 Adding helm repositories: jetstack, kcp"
helm repo add jetstack https://charts.jetstack.io
helm repo add kcp https://kcp-dev.github.io/helm-charts
helm repo update

echodate "⬇️ Installing cert-manager…"

kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.crds.yaml"
helm upgrade \
  --install \
  --wait \
  --namespace cert-manager \
  --create-namespace \
  --version "${CERT_MANAGER_VERSION}" \
  cert-manager jetstack/cert-manager

# Installing cert-manager will end with a message saying that the next step
# is to create some Issuers and/or ClusterIssuers.  That is indeed
# among the things that the kcp helm chart will do.

echodate "⚙️ Installing kcp…"

helm upgrade \
  --install \
  --wait \
  --values hack/kind-kcp/kcp-values.yaml \
  --namespace kcp \
  --create-namespace \
  --version "${KCP_CHART_VERSION}" \
  kcp kcp/kcp

echodate "🔐 Generating KCP admin kubeconfig…"
cat << EOF > "${KCP_ADMIN_KUBECONFIG}"
apiVersion: v1
kind: Config
clusters:
  - cluster:
      insecure-skip-tls-verify: true
      server: "https://${kcp_hostname}:8443/clusters/root"
    name: kind-kcp
contexts:
  - context:
      cluster: kind-kcp
      user: kind-kcp
    name: kind-kcp
current-context: kind-kcp
users:
  - name: kind-kcp
    user:
      token: admin-token
EOF

echodate "🔍 Checking /etc/hosts for ${kcp_hostname}…"
if ! grep -q "$kcp_hostname" /etc/hosts; then
  update_hosts_lc="$(printf '%s' "$UPDATE_HOSTS" | tr '[:upper:]' '[:lower:]')"
  if [ "$update_hosts_lc" = "true" ]; then
    echo "127.0.0.1 $kcp_hostname" | sudo tee -a /etc/hosts
    echodate "✅ Added ${kcp_hostname} to /etc/hosts."
  else
    echodate "⚠️ ${kcp_hostname} not found in /etc/hosts and UPDATE_HOSTS is false."
    echodate "$(printf '\tTo add it manually, run as root:\n\t  sudo sh -c "echo \"127.0.0.1 %s\" >> /etc/hosts"\n' "$kcp_hostname")"
    echodate "$(printf '\tOr add this line to /etc/hosts:\n\t  127.0.0.1 %s\n' "$kcp_hostname")"
  fi
else
  echodate "✅ ${kcp_hostname} already exists in /etc/hosts."
fi
