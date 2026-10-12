#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Build distributable binaries with complete linked-dependency notices.
set -euo pipefail
cd "$(dirname "$0")/.."
version="${RAMET_RELEASE_VERSION:-0.3.0-alpha.1}"
destination="${RAMET_RELEASE_DIR:-.test-cache/release}"
mkdir -p "$destination"
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  platform="${target%/*}"
  architecture="${target#*/}"
  name="ramet-${version}-${platform}-${architecture}"
  stage=$(mktemp -d)
  mkdir "$stage/$name"
  CGO_ENABLED=0 GOOS="$platform" GOARCH="$architecture" go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$stage/$name/ramet" ./cmd/ramet
  cp LICENSE NOTICE THIRD_PARTY_NOTICES.md "$stage/$name/"
  cat > "$stage/$name/README.txt" <<'TXT'
Ramet: persistent, forkable workspaces for AI agents
Run ./ramet demo for a self-contained demonstration, or ./ramet serve to start
an HTTP worker. Git is required for Git imports. The optional lazy backend also
requires Linux, accessible /dev/fuse, and fusermount3.
Documentation and source: https://github.com/shenli/ramet
License: Apache-2.0. See LICENSE, NOTICE and THIRD_PARTY_NOTICES.md.
TXT
  cp -R third_party "$stage/$name/third_party"
  tar -czf "$destination/$name.tar.gz" -C "$stage" "$name"
  # Each stage is newly allocated solely for this script; retain it on failure.
  python3 -c 'import shutil,sys; shutil.rmtree(sys.argv[1])' "$stage"
done
python3 - "$destination" <<'PY'
import hashlib
from pathlib import Path
import sys
directory = Path(sys.argv[1])
with (directory / "SHA256SUMS").open("w") as output:
    for archive in sorted(directory.glob("ramet-*.tar.gz")):
        output.write(hashlib.sha256(archive.read_bytes()).hexdigest() + "  " + archive.name + "\n")
PY
