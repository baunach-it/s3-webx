# s3-webx

Docker image that keeps web-ready variants next to the images and videos in S3-compatible buckets.

> **Status: under construction.** The service is not usable yet.

## What it does

- Walks one or more S3-compatible buckets (Cloudflare R2, MinIO, Hetzner, AWS) at a regular interval.
- Converts every image to WebP and stores it next to the original with the extension appended:
  `foto.jpg` → `foto.jpg.webp`. GIFs become animated WebP.
- Converts every video to WebM (VP9 + Opus): `film.mp4` → `film.mp4.webm`.
- Keeps a variant only if it is smaller than the original. Originals are never modified or deleted.
- Reports its state over HTTP: health check, an overview page and a status API.

Images are converted with [libvips](https://www.libvips.org/), videos with [FFmpeg](https://ffmpeg.org/).

## Configuration

Follows. The service is configured by a single YAML file; secrets are referenced as `${VAR}` and resolved from the
environment at startup.

## Image

`baunach/s3-webx` on Docker Hub, `linux/amd64` only.
