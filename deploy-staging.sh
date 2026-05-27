#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

GCP_PROJECT_ID="${GCP_PROJECT_ID:-curelinkai}"
ARTIFACT_REPOSITORY_NAME="${ARTIFACT_REPOSITORY_NAME:-disha-voice-worker-staging}"
GKE_NAMESPACE="${GKE_NAMESPACE:-staging}"
TALK_GO_DEPLOYMENT_NAME="${TALK_GO_DEPLOYMENT_NAME:-disha-go-voice-worker-staging}"
TALK_GO_MIN_REPLICA_COUNT="${TALK_GO_MIN_REPLICA_COUNT:-1}"

timestamp="$(date -u +%Y%m%d%H%M%S)"
POD_TEMPLATE_VERSION="${POD_TEMPLATE_VERSION:-v${timestamp}}"
IMAGE_REPOSITORY="us-east1-docker.pkg.dev/${GCP_PROJECT_ID}/${ARTIFACT_REPOSITORY_NAME}/talk-go-worker"
IMAGE="${IMAGE_REPOSITORY}:latest"

required_commands=(docker kubectl envsubst)
for cmd in "${required_commands[@]}"; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing required command: $cmd" >&2
    exit 1
  fi
done

echo "Deploying TalkGo staging"
echo "  context: $(kubectl config current-context)"
echo "  namespace: ${GKE_NAMESPACE}"
echo "  deployment: ${TALK_GO_DEPLOYMENT_NAME}"
echo "  image: ${IMAGE}"

echo "Building image..."
docker build --platform=linux/amd64 -t "$IMAGE" .

echo "Pushing image..."
docker push "$IMAGE"

if [[ -f .prod.env ]]; then
  echo "Updating talk-go-worker-env secret from .prod.env..."
  kubectl create secret generic talk-go-worker-env \
    --namespace "$GKE_NAMESPACE" \
    --from-env-file=.prod.env \
    --dry-run=client -o yaml \
    | kubectl apply -f -
else
  echo "Skipping secret update: .prod.env not found"
fi

echo "Applying Kubernetes manifest..."
export GCP_PROJECT_ID
export ARTIFACT_REPOSITORY_NAME
export GKE_NAMESPACE
export TALK_GO_DEPLOYMENT_NAME
export TALK_GO_MIN_REPLICA_COUNT
export POD_TEMPLATE_VERSION

envsubst '$TALK_GO_DEPLOYMENT_NAME $ARTIFACT_REPOSITORY_NAME $GKE_NAMESPACE $GCP_PROJECT_ID $POD_TEMPLATE_VERSION $TALK_GO_MIN_REPLICA_COUNT' \
  < k8s/talk-go-worker-staging.yaml \
  | kubectl apply -f -

echo "Waiting for rollout..."
kubectl rollout status "deployment/${TALK_GO_DEPLOYMENT_NAME}" \
  --namespace "$GKE_NAMESPACE" \
  --timeout=10m

echo "Current pods:"
kubectl get pods \
  --namespace "$GKE_NAMESPACE" \
  -l "app=${TALK_GO_DEPLOYMENT_NAME}" \
  -o wide

echo "Done."
