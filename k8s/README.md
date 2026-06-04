# TalkGo GKE Worker

`worker-staging.yaml` deploys TalkGo as a Disha-compatible GKE worker.

Required deploy config in `.staging.env`:

```text
GKE_DEPLOYMENT_NAME=<deployment name, for example disha-go-voice-worker-staging>
```

The TalkGo deployment runs in the existing voice-worker cluster. The deployment
script reads `.staging.env` itself; do not export these in the shell. The deployment
name is separate from the Artifact Registry repository name, so staging can use:

```text
GKE_DEPLOYMENT_NAME=disha-go-voice-worker-staging
ARTIFACT_REPOSITORY_NAME=disha-voice-worker-staging
```

Required Kubernetes secret for TalkGo runtime env:

```bash
kubectl create secret generic talk-go-worker-env \
  --namespace "$GKE_NAMESPACE" \
  --dry-run=client -o yaml \
  --from-env-file=.staging.env \
  | kubectl apply -f -
```

Keep runtime env in `.staging.env`, including `FAST_API_PORT=7860`, Pyroscope
credentials, and `PERF_DIAGNOSTICS_ENABLED`. The manifest injects
`GKE_DEPLOYMENT_NAME` directly so the worker registration name matches the
Kubernetes deployment name. The S3 env values should match the Python worker
because TalkGo uploads the same `debug_log_data/{conversation_id}/log_data.json`
object.

The manifest mirrors Disha backend `k8s/worker.yaml` for worker scheduling,
resources, KEDA behavior, and PDB settings, except the Postgres scaler query
uses the TalkGo deployment's active/reserved/provisioning worker count. The
deploy script reads `DB_USER`, `DB_PASSWORD`, `DB_HOST`, `DB_PORT`, and
`DB_NAME` from `.staging.env` so it can create the KEDA Postgres connection Secret.

Build, push, and deploy staging:

```bash
./deploy-staging.sh
```

The script defaults to:

```text
GCP_PROJECT_ID=curelinkai
GKE_NAMESPACE=staging
GKE_DEPLOYMENT_NAME=disha-go-voice-worker-staging
ARTIFACT_REPOSITORY_NAME=disha-voice-worker-staging
```

It builds and pushes `latest`, refreshes `talk-go-worker-env` from local
`.staging.env`, applies the manifest with a fresh `POD_TEMPLATE_VERSION`, and waits
for rollout. The template-version label is what forces Kubernetes to replace
pods and pull the newly pushed `latest` image.

Set `PERF_DIAGNOSTICS_ENABLED=1` in `.staging.env` for a profiled staging run. That
single flag enables Go/Python Pyroscope startup, `process_usage`, and
`audio_timing`. Leave it at `0` for normal runs so the 20ms audio hot path skips
timing instrumentation.
