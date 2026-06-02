# TalkGo GKE Worker

`talk-go-worker-staging.yaml` deploys TalkGo as a Disha-compatible GKE worker.

Required Disha backend env:

```text
TALK_GO_DEPLOYMENT_NAME=<deployment name, for example disha-go-voice-worker-staging>
```

The TalkGo deployment runs in the existing voice-worker cluster. The deployment
name is separate from the Artifact Registry repository name, so staging can use:

```text
TALK_GO_DEPLOYMENT_NAME=disha-go-voice-worker-staging
ARTIFACT_REPOSITORY_NAME=disha-voice-worker-staging
```

Required Kubernetes secret for TalkGo runtime env:

```bash
kubectl create secret generic talk-go-worker-env \
  --namespace "$GKE_NAMESPACE" \
  --dry-run=client -o yaml \
  --from-literal=DISHA_API_URL="$DISHA_API_URL" \
  --from-literal=DISHA_REDIS_URL="$DISHA_REDIS_URL" \
  --from-literal=DISHA_REDIS_PASSWORD="$DISHA_REDIS_PASSWORD" \
  --from-literal=OPENAI_API_KEY="$OPENAI_API_KEY" \
  --from-literal=SONIOX_API_KEY="$SONIOX_API_KEY" \
  --from-literal=CARTESIA_API_KEY="$CARTESIA_API_KEY" \
  --from-literal=ACCESS_KEY_ID="$ACCESS_KEY_ID" \
  --from-literal=SECRET_KEY_ID="$SECRET_KEY_ID" \
  --from-literal=AWS_MAIN_REGION="$AWS_MAIN_REGION" \
  --from-literal=AWS_BUCKET_NAME="$AWS_BUCKET_NAME" \
  | kubectl apply -f -
```

Also include `REDIS_DB` if the target Redis database is not `0`. The S3 env
values should match the Python worker because TalkGo uploads the same
`debug_log_data/{conversation_id}/log_data.json` object.

The manifest reuses the existing voice-worker KEDA `postgresql-trigger-auth`
object in the same namespace.

Build, push, and deploy staging:

```bash
./deploy-staging.sh
```

The script defaults to:

```text
GCP_PROJECT_ID=curelinkai
GKE_NAMESPACE=staging
TALK_GO_DEPLOYMENT_NAME=disha-go-voice-worker-staging
ARTIFACT_REPOSITORY_NAME=disha-voice-worker-staging
TALK_GO_MIN_REPLICA_COUNT=1
PERF_DIAGNOSTICS_ENABLED=0
```

It builds and pushes `latest`, optionally refreshes `talk-go-worker-env` from
local `.prod.env`, applies the manifest with a fresh `POD_TEMPLATE_VERSION`, and
waits for rollout. The template-version label is what forces Kubernetes to
replace pods and pull the newly pushed `latest` image.

Set `PERF_DIAGNOSTICS_ENABLED=1` for a profiled staging run. That single flag
enables Go/Python Pyroscope startup, `process_usage`, and `audio_timing`. Leave it
at `0` for normal runs so the 20ms audio hot path skips timing instrumentation.
