# s3-webx

Docker image that keeps web-ready variants next to the images and videos in S3-compatible buckets.

## What it does

- Walks one or more S3-compatible buckets (Cloudflare R2, MinIO, Hetzner, AWS) at a regular interval.
- Converts every image to WebP and stores it next to the original with the extension appended:
  `foto.jpg` → `foto.jpg.webp`. GIFs become animated WebP.
- Converts every video to WebM (VP9 + Opus): `film.mp4` → `film.mp4.webm`.
- Keeps every variant by default, even one larger than the original, so a website can always link the `.webp` or
  `.webm`. With `keep_if_larger: false` a variant that is not smaller is skipped. Originals are never modified or
  deleted.
- Reports its state over HTTP: health check, an overview page and a status API.

Images are converted with [libvips](https://www.libvips.org/), videos with [FFmpeg](https://ffmpeg.org/).

## Quickstart

Write a configuration, for example `s3-webx.yaml` for one MinIO bucket:

```yaml
listen: ":8080"
state: /data/state.db
interval: 15m

auth:
  ui_user: admin
  ui_password: ${S3WEBX_UI_PASSWORD}
  api_token: ${S3WEBX_API_TOKEN}

buckets:
  - name: content
    endpoint: http://minio:9000
    region: us-east-1
    bucket: content
    path_style: true
    access_key: ${S3WEBX_ACCESS_KEY}
    secret_key: ${S3WEBX_SECRET_KEY}
```

Run it with the secrets in the environment and a volume for the state:

```sh
docker run -d --name s3-webx \
  -p 8080:8080 \
  -v s3-webx-data:/data \
  -v "$PWD/s3-webx.yaml:/config/s3-webx.yaml:ro" \
  -e S3WEBX_UI_PASSWORD -e S3WEBX_API_TOKEN \
  -e S3WEBX_ACCESS_KEY -e S3WEBX_SECRET_KEY \
  baunach/s3-webx:latest
```

The first scan starts right away; `http://localhost:8080/` shows the progress (Basic Auth `admin`).

| Item | Value |
|---|---|
| Configuration | `/config/s3-webx.yaml` (override with `-config <path>` as container command) |
| Volume | `/data`: `state.db` and the work folder `tmp/` for downloads and conversions |
| Port | `8080`; the `HEALTHCHECK` calls `http://127.0.0.1:8080/healthz` and assumes that port |
| User | UID/GID `10001`; a bind mount for `/data` must be writable for it |
| Logs | JSON on stdout, one line per file with `bytes_before`, `bytes_after` and `duration` |

The access key needs read and write access to its bucket only: list, get, put and the object metadata.

## Configuration

The service is configured by a single YAML file, passed with `-config` (default `/config/s3-webx.yaml`). Secrets are
referenced as `${VAR}` and resolved from the environment at startup; an unset variable or an unknown key stops the
service. All keys with their defaults: [`config.example.yaml`](config.example.yaml).

`keep_if_larger` (default `true`, per bucket or in `defaults`) uploads the variant even when it is not smaller than the
original. Set it to `false` to keep only variants that save bytes; such files end as `skipped`. While it is `true`, a
file once skipped is converted again on the next scan.

`state.db` follows the buckets. At startup the service drops the entries of buckets that are no longer configured;
their objects stay untouched. A scan records a variant that is already in the bucket and not older than its original
as `done` when `state.db` has no entry for that original, for example after the volume was replaced. Such an entry
carries the size and time of the variant and a duration of 0.

## HTTP

The service listens on `listen` (default `:8080`).

| Endpoint | Auth | Answer |
|---|---|---|
| `GET /healthz` | none | `200 ok` when the state database answers, otherwise `503` |
| `GET /` | Basic (`auth.ui_user`, `auth.ui_password`) | HTML overview, refreshes every 10 s |
| `GET /assets/app.css` | Basic | stylesheet of the overview |
| `GET /api/status` | Bearer (`auth.api_token`) | workers and per-bucket counters |
| `GET /api/buckets/{name}/files` | Bearer | entries of one bucket, newest first |
| `GET /api/buckets/{name}/files/{key}` | Bearer | state of one original, incl. whether its variant exists |

- A missing or wrong credential gets `401` with a `WWW-Authenticate` challenge (`Basic realm="s3-webx"` or
  `Bearer realm="s3-webx"`). An empty `ui_user`/`ui_password` or `api_token` disables that access with `403`; it is
  never open.
- API answers are JSON with camelCase fields; errors are `{"error": "..."}`. An unknown bucket or file is `404`, an
  invalid parameter `400`.
- `files` takes `status` (`queued`, `running`, `done`, `skipped`, `failed`; repeat it or separate with commas),
  `kind` (`image`, `video`), `limit` (1–1000, default 100) and `offset`.
- The key in `files/{key}` is the original's full key, URL-encoded per segment:
  `/api/buckets/content/files/aktion/2026-10-rstorc/Foto%201.JPG`.
- The overview shows the saved storage, counters per status, both workers, one card per bucket, and two tables with
  25 rows per page: recent conversions (`done`, `skipped`) and failures. `?recent=N&failures=M` selects the pages; the
  links keep the other table's page, and a number that is not a page falls back to the first or last one.
- The overview runs no script and loads only its own stylesheet:
  `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self' data:; frame-ancestors 'none'`. The
  stylesheet is linked with a content hash (`/assets/app.css?v=…`) and cached for a year
  (`Cache-Control: private, max-age=31536000, immutable`).
- An empty original (0 bytes) is marked `failed` with `source object is empty (0 bytes)` at once and is not retried
  until its object changes.

```sh
curl -H "Authorization: Bearer $TOKEN" https://s3-webx.example/api/status
```

```json
{
  "workers": [
    {"kind": "image", "idle": true},
    {"kind": "video", "idle": false, "bucket": "content", "key": "film.mp4",
     "startedAt": "2026-10-29T09:30:00Z", "percent": 42.5}
  ],
  "buckets": [
    {"name": "content", "lastScan": "2026-10-29T09:30:00Z", "queued": 1,
     "counts": {"image": {"done": 1, "failed": 0, "queued": 1, "running": 0, "skipped": 1},
                "video": {"done": 1, "failed": 1, "queued": 0, "running": 0, "skipped": 0}},
     "bytesBefore": 2000, "bytesAfter": 500, "bytesSaved": 1500}
  ]
}
```

`lastScan` is `null` before the first scan; a failed scan adds `lastScanError`. The byte counters sum the entries
that are `done`, i.e. whose variant is in the bucket.

```sh
curl -H "Authorization: Bearer $TOKEN" \
  "https://s3-webx.example/api/buckets/content/files/aktion/2026-10-rstorc/Foto%201.JPG"
```

```json
{"bucket": "content", "key": "aktion/2026-10-rstorc/Foto 1.JPG", "kind": "image", "status": "done",
 "sourceModified": "2026-10-29T08:00:00Z", "sourceSize": 9472360,
 "variantKey": "aktion/2026-10-rstorc/Foto 1.JPG.webp", "variantSize": 687692, "variantAvailable": true,
 "attempts": 0, "durationMs": 2140,
 "queuedAt": "2026-10-29T08:05:00Z", "updatedAt": "2026-10-29T08:05:03Z"}
```

`variantAvailable` is true only for `done`. A `skipped` entry carries the measured `variantSize` of the variant that
was not uploaded; a `failed` entry adds `error`. The list endpoint answers `{"total", "limit", "offset", "files": [...]}`
with the same file objects.

## Limits

- One image worker and one video worker, each converting one file at a time across all buckets. A long video does not
  hold up images, but a second video waits for the first.
- Each original is downloaded completely into `/data/tmp` before it is converted; the volume needs room for the largest
  original plus its variant.
- Nothing is ever deleted: a variant stays when its original is removed or renamed.
- Videos are slow: in the sandbox test a 16-minute 1680×1050 H.264 video took almost 14 minutes (measurements in
  `vault/concepts/funktionsweise.md`).
- The HTTP layer has no TLS; put it behind a reverse proxy or tunnel when it leaves the host.
- Keys with `.`, `..` or empty segments are converted, but cannot be queried through `files/{key}`.

## Image

`baunach/s3-webx` on Docker Hub, `linux/amd64` only, tags `latest` and the full commit SHA. Alpine with libvips and
FFmpeg from the distribution, a static Go binary, non-root user `10001`.

The stylesheet is built from `internal/web/tailwind.css` and the template with the Tailwind CSS standalone CLI
(v4.3.3, checksum pinned) in its own stage; locally `./dev css` does the same, and `./dev build`, `test` and `lint` run
it when `internal/web/app.css` is missing or older than its sources. The generated file is not committed.

The build stage runs `go vet` and all tests, including the converter tests against the real `vips` and `ffmpeg`; a
failing or skipped test stops the build. Locally: `./dev image` builds `baunach/s3-webx:local`.

CI (`.github/workflows/ci.yaml`) has two jobs. `test` runs for pull requests on Gitea and only builds and tests the
image, without secrets and without pushing. `release` runs only on the GitHub mirror: a push to `main`, a manual
dispatch and every Sunday at 03:00 UTC build and push the image (secrets `DOCKER_USERNAME`, `DOCKER_ACCESSTOKEN_RW`).
Gitea reads the same workflow, but sets `github.server_url` to its own address and therefore never releases.

## License

The code of `s3-webx` is licensed under [MIT-0](LICENSE).

The image also contains third-party software under its own licenses:

| Part | License | Source |
|---|---|---|
| FFmpeg (Alpine package `ffmpeg`, GPL build) | GPL-2.0-or-later AND LGPL-2.1-or-later | Alpine package sources, https://gitlab.alpinelinux.org/alpine/aports |
| libvips (`vips`, `vips-tools`) | LGPL-2.1-or-later | Alpine package sources, https://gitlab.alpinelinux.org/alpine/aports |
| Alpine base (`musl`, `busybox`, `alpine-baselayout`, …) | per package, e.g. MIT (`musl`), GPL-2.0-only (`busybox`) | `apk list -I` in the image lists every package with its license |
| Go standard library and the Go modules linked into the binary | BSD-3-Clause, BSD-2-Clause, MIT, Apache-2.0, public domain (SQLite) | texts in `/usr/share/licenses/s3-webx/` |

Go modules in the binary:

| Module | License |
|---|---|
| `github.com/minio/minio-go/v7`, `github.com/minio/crc64nvme`, `github.com/minio/md5-simd`, `gopkg.in/ini.v1` | Apache-2.0 (`md5-simd` also BSD-3-Clause for code from Go) |
| `gopkg.in/yaml.v3`, `go.yaml.in/yaml/v3` | MIT and Apache-2.0 |
| `modernc.org/sqlite` | BSD-3-Clause; contains SQLite (public domain) and sqlite-vec (MIT) |
| `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory` | BSD-3-Clause; `libc` contains code from Go (BSD-3-Clause), musl and go-netdb (MIT) |
| `golang.org/x/crypto`, `golang.org/x/net`, `golang.org/x/sys`, `golang.org/x/text`, `github.com/google/uuid`, `github.com/klauspost/compress`, `github.com/klauspost/crc32`, `github.com/remyoudompheng/bigfft` | BSD-3-Clause |
| `github.com/zeebo/xxh3` | BSD-2-Clause |
| `github.com/cespare/xxhash/v2`, `github.com/dustin/go-humanize`, `github.com/klauspost/cpuid/v2`, `github.com/philhofer/fwd`, `github.com/rs/xid`, `github.com/tinylib/msgp` | MIT |

`s3-webx` runs `ffmpeg` and `vips` as separate processes and does not link them. The image build copies the own
`LICENSE`, the Go license and the license, notice and patent files of every linked module to
`/usr/share/licenses/s3-webx/`; a module without a license file stops the build.
