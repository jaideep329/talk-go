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
  --from-env-file=.prod.env \
  | kubectl apply -f -
```

Keep runtime env in `.prod.env`, including `FAST_API_PORT=7860`,
`GKE_DEPLOYMENT_NAME`, Pyroscope credentials, and
`PERF_DIAGNOSTICS_ENABLED`. The S3 env values should match the Python worker
because TalkGo uploads the same `debug_log_data/{conversation_id}/log_data.json`
object.

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
```

It builds and pushes `latest`, refreshes `talk-go-worker-env` from local
`.prod.env`, applies the manifest with a fresh `POD_TEMPLATE_VERSION`, and waits
for rollout. The template-version label is what forces Kubernetes to replace
pods and pull the newly pushed `latest` image.

Set `PERF_DIAGNOSTICS_ENABLED=1` in `.prod.env` for a profiled staging run. That
single flag enables Go/Python Pyroscope startup, `process_usage`, and
`audio_timing`. Leave it at `0` for normal runs so the 20ms audio hot path skips
timing instrumentation.
