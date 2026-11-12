FROM golang:1.27.2-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ramet ./cmd/ramet

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 10001 --create-home ramet \
    && mkdir /data && chown ramet:ramet /data
COPY --from=build /ramet /usr/local/bin/ramet
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/doc/ramet/
COPY third_party/ /usr/share/doc/ramet/third_party/
USER ramet
WORKDIR /home/ramet
ENV RAMET_DATA=/data RAMET_LISTEN=0.0.0.0:7433
EXPOSE 7433
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s \
    CMD ["ramet", "health"]
ENTRYPOINT ["ramet"]
CMD ["serve"]
