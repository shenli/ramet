# SPDX-License-Identifier: Apache-2.0
FROM python:3.12-slim-bookworm
COPY requirements.lock /opt/requirements.lock
RUN python -m pip download --only-binary=:all: --require-hashes \
    --dest /opt/wheels -r /opt/requirements.lock
# Packages are intentionally not installed globally: the example persists them
# in the workspace. Run the built image by its immutable ID or registry digest.
