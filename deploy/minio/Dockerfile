# SPDX-License-Identifier: Apache-2.0
# Separate, unmodified AGPL-3.0 services; not linked into Ramet.
FROM golang:1.26.9-bookworm AS server-build
RUN git clone https://github.com/minio/minio.git /src && \
    cd /src && git checkout 9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a && \
    CGO_ENABLED=0 go build -trimpath -o /out/minio .

FROM golang:1.26.9-bookworm AS client-build
RUN git clone https://github.com/minio/mc.git /src && \
    cd /src && git checkout d6541ea280b73a834b64d4097e21f2be77676104 && \
    CGO_ENABLED=0 go build -trimpath -o /out/mc .

FROM debian:bookworm-slim AS base
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY README.md /usr/share/doc/minio/SOURCE.md

FROM base AS server
COPY --from=server-build /out/minio /usr/local/bin/minio
COPY --from=server-build /src/LICENSE /usr/share/doc/minio/LICENSE
EXPOSE 9000
ENTRYPOINT ["minio"]
CMD ["server", "/data"]

FROM base AS client
COPY --from=client-build /out/mc /usr/local/bin/mc
COPY --from=client-build /src/LICENSE /usr/share/doc/minio/LICENSE
ENTRYPOINT ["mc"]
