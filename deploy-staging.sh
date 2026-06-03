#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

GCP_PROJECT_ID="${GCP_PROJECT_ID:-curelinkai}"
ARTIFACT_REPOSITORY_NAME="${ARTIFACT_REPOSITORY_NAME:-disha-voice-worker-staging}"
GKE_NAMESPACE="${GKE_NAMESPACE:-staging}"
TALK_GO_DEPLOYMENT_NAME="${TALK_GO_DEPLOYMENT_NAME:-disha-go-voice-worker-staging}"
TALK_GO_MIN_REPLICA_COUNT="${TALK_GO_MIN_REPLICA_COUNT:-1}"

# Target cluster is pinned here so the deploy NEVER follows whatever the local
# kubectl current-context happens to be. All kubectl calls below run against
# "$KUBE_CONTEXT" explicitly.
GKE_CLUSTER_NAME="${GKE_CLUSTER_NAME:-disha-voice-worker-staging}"
GKE_CLUSTER_LOCATION="${GKE_CLUSTER_LOCATION:-us-east1}"
KUBE_CONTEXT="gke_${GCP_PROJECT_ID}_${GKE_CLUSTER_LOCATION}_${GKE_CLUSTER_NAME}"

timestamp="$(date -u +%Y%m%d%H%M%S)"
POD_TEMPLATE_VERSION="${POD_TEMPLATE_VERSION:-v${timestamp}}"
IMAGE_REPOSITORY="us-east1-docker.pkg.dev/${GCP_PROJECT_ID}/${ARTIFACT_REPOSITORY_NAME}/talk-go-worker"
IMAGE="${IMAGE_REPOSITORY}:latest"

required_commands=(docker kubectl envsubst gcloud)
for cmd in "${required_commands[@]}"; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required command: $cmd" >&2
    exit 1
  fi
done

# Ensure a kubeconfig entry for the target cluster exists / is fresh. This
# creates the context named "$KUBE_CONTEXT" without changing the user's active
# context — every kubectl call below passes --context explicitly anyway.
echo "Fetching credentials for cluster ${GKE_CLUSTER_NAME} (${GKE_CLUSTER_LOCATION})..."
gcloud container clusters get-credentials "$GKE_CLUSTER_NAME" \
  --region "$GKE_CLUSTER_LOCATION" \
  --project "$GCP_PROJECT_ID"

# Hard guard: refuse to proceed unless the pinned context actually exists.
if ! kubectl config get-contexts -o name | grep -qx "$KUBE_CONTEXT"; then
  echo "Expected kube context '${KUBE_CONTEXT}' not found after get-credentials." >&2
  echo "Check GKE_CLUSTER_NAME / GKE_CLUSTER_LOCATION / GCP_PROJECT_ID." >&2
  exit 1
fi

echo "Deploying TalkGo staging"
echo "  context: ${KUBE_CONTEXT} (pinned)"
echo "  namespace: ${GKE_NAMESPACE}"
echo "  deployment: ${TALK_GO_DEPLOYMENT_NAME}"
echo "  image: ${IMAGE}"

echo "Building image..."
docker build --platform=linux/amd64 -t "$IMAGE" .

echo "Pushing image..."
docker push "$IMAGE"

if [[ ! -f .prod.env ]]; then
  echo ".prod.env is required for staging deploys because runtime env is loaded from talk-go-worker-env." >&2
  exit 1
fi
echo "Updating talk-go-worker-env secret from .prod.env..."
kubectl --context "$KUBE_CONTEXT" create secret generic talk-go-worker-env \
  --namespace "$GKE_NAMESPACE" \
  --from-env-file=.prod.env \
  --dry-run=client -o yaml \
  | kubectl --context "$KUBE_CONTEXT" apply -f -

echo "Applying Kubernetes manifest..."
export GCP_PROJECT_ID
export ARTIFACT_REPOSITORY_NAME
export GKE_NAMESPACE
export TALK_GO_DEPLOYMENT_NAME
export TALK_GO_MIN_REPLICA_COUNT
export POD_TEMPLATE_VERSION

envsubst '$TALK_GO_DEPLOYMENT_NAME $ARTIFACT_REPOSITORY_NAME $GKE_NAMESPACE $GCP_PROJECT_ID $POD_TEMPLATE_VERSION $TALK_GO_MIN_REPLICA_COUNT' \
  < k8s/talk-go-worker-staging.yaml \
  | kubectl --context "$KUBE_CONTEXT" apply -f -

echo "Waiting for rollout..."
kubectl --context "$KUBE_CONTEXT" rollout status "deployment/${TALK_GO_DEPLOYMENT_NAME}" \
  --namespace "$GKE_NAMESPACE" \
  --timeout=10m

echo "Current pods:"
kubectl --context "$KUBE_CONTEXT" get pods \
  --namespace "$GKE_NAMESPACE" \
  -l "app=${TALK_GO_DEPLOYMENT_NAME}" \
  -o wide

echo "Done."
