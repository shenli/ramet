# Reference object-store images

The Compose examples build unmodified upstream MinIO and its client locally.
Previously pinned public registry images no longer permit anonymous pulls.
The first build requires internet access and can take several minutes.

- Server: [RELEASE.2025-10-15T17-29-55Z](https://github.com/minio/minio/tree/9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a).
- Client: [RELEASE.2025-08-13T08-35-41Z](https://github.com/minio/mc/tree/d6541ea280b73a834b64d4097e21f2be77676104).

Both are separate AGPL-3.0 services. Each image includes the upstream license
and this source reference. Ramet remains Apache-2.0; these services are not
bundled in Ramet release archives. Review upstream licensing and security
maintenance before operating or redistributing your own object-store service.
You can configure Ramet against an existing S3-compatible service instead.
