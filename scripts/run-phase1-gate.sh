#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Run on a Linux Docker host, from a dedicated test checkout.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p .test-cache docs/benchmarks
docker compose -p ramet-phase1-test -f compose.integration.yaml up -d --wait
docker run --rm --network host --user "$(id -u):$(id -g)" \
  -v "$PWD:/work" -w /work \
  -e GOCACHE=/work/.test-cache/build -e GOMODCACHE=/work/.test-cache/mod \
  golang:1.26.9-bookworm bash -ec '
    go test -race -count=1 ./...
    bash scripts/test-integration.sh
    CGO_ENABLED=0 go build -trimpath -o .test-cache/ramet ./cmd/ramet
  '

export RAMET_TOKEN=ramet-local-integration-only
export RAMET_DATABASE_URL='postgres://ramet_test:ramet-local-integration-only@127.0.0.1:17432/ramet_phase1_test?sslmode=disable'
export RAMET_S3_ACCESS_KEY=ramet_test
export RAMET_S3_SECRET_KEY=ramet-local-integration-only
export RAMET_URL=http://127.0.0.1:17433
export RAMET_TEST_WORKDIR="$PWD/.test-cache"
namespace="fanout-$(date +%s)-$$"
cat > .test-cache/fanout.yaml <<EOF
server:
  listen: "127.0.0.1:17433"
  token_env: RAMET_TOKEN
worker:
  data: "$PWD/.test-cache/worker-data-$namespace"
  max_executions: 2048
  max_pools: 8
metadata:
  provider: postgres
  dsn_env: RAMET_DATABASE_URL
  namespace: $namespace
content:
  provider: s3
  endpoint: http://127.0.0.1:17900
  region: us-east-1
  bucket: ramet-phase1-integration
  prefix: $namespace/objects
  access_key_env: RAMET_S3_ACCESS_KEY
  secret_key_env: RAMET_S3_SECRET_KEY
EOF
.test-cache/ramet migrate --config .test-cache/fanout.yaml
.test-cache/ramet serve --config .test-cache/fanout.yaml > .test-cache/worker.log 2>&1 &
worker_pid=$!
trap 'kill "$worker_pid" 2>/dev/null || true; wait "$worker_pid" 2>/dev/null || true' EXIT
for attempt in $(seq 1 60); do
  if .test-cache/ramet health >/dev/null 2>&1; then break; fi
  if ! kill -0 "$worker_pid" 2>/dev/null; then cat .test-cache/worker.log >&2; exit 1; fi
  sleep 1
done
.test-cache/ramet health
PYTHONPATH=sdk/python python3 scripts/phase1-fanout.py \
  --concurrency "${RAMET_FANOUT_CONCURRENCY:-1000}" --rounds "${RAMET_FANOUT_ROUNDS:-3}" \
  --output docs/benchmarks/phase1-linux-fanout.json
.test-cache/ramet drain
if .test-cache/ramet health >/dev/null 2>&1; then
  echo 'drained worker incorrectly reports ready' >&2
  exit 1
fi
echo 'Phase 1 gate passed.'
