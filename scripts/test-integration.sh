#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail
cd "$(dirname "$0")/.."
export RAMET_TEST_POSTGRES="${RAMET_TEST_POSTGRES:-postgres://ramet_test:ramet-local-integration-only@127.0.0.1:17432/ramet_phase1_test?sslmode=disable}"
export RAMET_TEST_S3_ENDPOINT="${RAMET_TEST_S3_ENDPOINT:-http://127.0.0.1:17900}"
export RAMET_TEST_S3_BUCKET="${RAMET_TEST_S3_BUCKET:-ramet-phase1-integration}"
export RAMET_TEST_S3_ACCESS_KEY="${RAMET_TEST_S3_ACCESS_KEY:-ramet_test}"
export RAMET_TEST_S3_SECRET_KEY="${RAMET_TEST_S3_SECRET_KEY:-ramet-local-integration-only}"
go test -race -count=1 -v ./integration
