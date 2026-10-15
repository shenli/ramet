# Third-party notices

Ramet source is licensed under Apache-2.0. The worker links the components below.
Versions are pinned in `go.mod`; original license and notice texts are included.

| Component | Version | Included licenses and notices |
| --- | --- | --- |
| Go runtime and standard library | 1.26.9 minimum | [License](third_party/go-LICENSE) |
| `github.com/cespare/xxhash/v2` | v2.3.0 | [LICENSE.txt](third_party/github.com-cespare-xxhash-v2/LICENSE.txt) |
| `github.com/dustin/go-humanize` | v1.0.1 | [LICENSE](third_party/github.com-dustin-go-humanize/LICENSE) |
| `github.com/hanwen/go-fuse/v2` | v2.10.1 (Linux) | [LICENSE](third_party/github.com-hanwen-go-fuse-v2/LICENSE) |
| `github.com/google/uuid` | v1.6.0 | [LICENSE](third_party/github.com-google-uuid/LICENSE) |
| `github.com/jackc/pgpassfile` | v1.0.0 | [LICENSE](third_party/github.com-jackc-pgpassfile/LICENSE) |
| `github.com/jackc/pgservicefile` | v0.0.0-20240606120523-5a60cdf6a761 | [LICENSE](third_party/github.com-jackc-pgservicefile/LICENSE) |
| `github.com/jackc/pgx/v5` | v5.9.2 | [LICENSE](third_party/github.com-jackc-pgx-v5/LICENSE) |
| `github.com/klauspost/compress` | v1.19.2 | [LICENSE](third_party/github.com-klauspost-compress/LICENSE), [gzhttp/LICENSE](third_party/github.com-klauspost-compress/gzhttp-LICENSE), [internal/lz4ref/LICENSE](third_party/github.com-klauspost-compress/internal-lz4ref-LICENSE), [internal/snapref/LICENSE](third_party/github.com-klauspost-compress/internal-snapref-LICENSE), [s2/LICENSE](third_party/github.com-klauspost-compress/s2-LICENSE), [s2/cmd/internal/filepathx/LICENSE](third_party/github.com-klauspost-compress/s2-cmd-internal-filepathx-LICENSE), [s2/cmd/internal/readahead/LICENSE](third_party/github.com-klauspost-compress/s2-cmd-internal-readahead-LICENSE), [snappy/LICENSE](third_party/github.com-klauspost-compress/snappy-LICENSE), [snappy/xerial/LICENSE](third_party/github.com-klauspost-compress/snappy-xerial-LICENSE), [zstd/internal/xxhash/LICENSE.txt](third_party/github.com-klauspost-compress/zstd-internal-xxhash-LICENSE.txt) |
| `github.com/klauspost/cpuid/v2` | v2.4.0 | [LICENSE](third_party/github.com-klauspost-cpuid-v2/LICENSE) |
| `github.com/klauspost/crc32` | v1.3.0 | [LICENSE](third_party/github.com-klauspost-crc32/LICENSE) |
| `github.com/minio/crc64nvme` | v1.1.1 | [LICENSE](third_party/github.com-minio-crc64nvme/LICENSE) |
| `github.com/minio/md5-simd` | v1.1.2 | [LICENSE](third_party/github.com-minio-md5-simd/LICENSE), [LICENSE.Golang](third_party/github.com-minio-md5-simd/LICENSE.Golang) |
| `github.com/minio/minio-go/v7` | v7.3.0 | [LICENSE](third_party/github.com-minio-minio-go-v7/LICENSE), [NOTICE](third_party/github.com-minio-minio-go-v7/NOTICE) |
| `github.com/philhofer/fwd` | v1.2.0 | [LICENSE.md](third_party/github.com-philhofer-fwd/LICENSE.md) |
| `github.com/restic/chunker` | v0.5.0 | [LICENSE](third_party/github.com-restic-chunker/LICENSE) |
| `github.com/rs/xid` | v1.6.0 | [LICENSE](third_party/github.com-rs-xid/LICENSE) |
| `github.com/tinylib/msgp` | v1.6.4 | [LICENSE](third_party/github.com-tinylib-msgp/LICENSE) |
| `github.com/zeebo/xxh3` | v1.1.0 | [LICENSE](third_party/github.com-zeebo-xxh3/LICENSE) |
| `go.yaml.in/yaml/v3` | v3.0.5 | [LICENSE](third_party/go.yaml.in-yaml-v3/LICENSE), [NOTICE](third_party/go.yaml.in-yaml-v3/NOTICE) |
| `golang.org/x/crypto` | v0.57.0 | [LICENSE](third_party/golang.org-x-crypto/LICENSE), [PATENTS](third_party/golang.org-x-crypto/PATENTS) |
| `golang.org/x/net` | v0.60.0 | [LICENSE](third_party/golang.org-x-net/LICENSE), [PATENTS](third_party/golang.org-x-net/PATENTS) |
| `golang.org/x/sys` | v0.48.0 | [LICENSE](third_party/golang.org-x-sys/LICENSE), [PATENTS](third_party/golang.org-x-sys/PATENTS) |
| `golang.org/x/text` | v0.42.0 | [LICENSE](third_party/golang.org-x-text/LICENSE), [PATENTS](third_party/golang.org-x-text/PATENTS) |
| `gopkg.in/ini.v1` | v1.67.3 | [LICENSE](third_party/gopkg.in-ini.v1/LICENSE) |

The Python SDK has no third-party runtime package dependencies. Build and test
tools are installed separately and are not bundled in the SDK wheel. Container
base packages, including Git, retain the notices supplied by their distribution.
PostgreSQL and MinIO are separate services in the reference deployment and retain
their upstream licensing; their source is not included in Ramet.
