#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Run from a dedicated Linux checkout, with Docker, /dev/fuse and fusermount3.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p .test-cache docs/benchmarks
docker compose -p ramet-phase2-test -f compose.integration.yaml up -d --wait
docker run --rm --network host --user "$(id -u):$(id -g)" \
  -v "$PWD:/work" -w /work \
  -e GOCACHE=/work/.test-cache/build -e GOMODCACHE=/work/.test-cache/mod \
  golang:1.26.9-bookworm bash -ec '
    go test -race -count=1 ./...
    go vet ./...
    go test -c -race -o .test-cache/engine.test ./engine
    go test -c -race -o .test-cache/integration.test ./integration
    CGO_ENABLED=0 go build -trimpath -o .test-cache/ramet ./cmd/ramet
  '
export RAMET_TEST_FUSE=1
export RAMET_TEST_POSTGRES='postgres://ramet_test:ramet-local-integration-only@127.0.0.1:17432/ramet_phase1_test?sslmode=disable'
export RAMET_TEST_S3_ENDPOINT=http://127.0.0.1:17900
export RAMET_TEST_S3_BUCKET=ramet-phase1-integration
export RAMET_TEST_S3_ACCESS_KEY=ramet_test
export RAMET_TEST_S3_SECRET_KEY=ramet-local-integration-only
.test-cache/engine.test -test.v -test.run TestLazyFilesystem -test.timeout 2m
.test-cache/integration.test -test.v -test.timeout 10m
PYTHONPATH=sdk/python RAMET_TEST_BINARY="$PWD/.test-cache/ramet" \
  python3 -m unittest discover -s sdk/python/tests -v
PYTHONPATH=sdk/python python3 scripts/phase2-soak.py \
  --duration "${RAMET_SOAK_SECONDS:-1800}" \
  --concurrency "${RAMET_SOAK_CONCURRENCY:-8}" \
  --output docs/benchmarks/phase2-linux-soak.json
echo 'Phase 2 gate passed. Stop dedicated services with docker compose -p ramet-phase2-test -f compose.integration.yaml down.'
