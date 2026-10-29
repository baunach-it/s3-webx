FROM mcr.microsoft.com/devcontainers/go:1.26-bookworm

# vips and ffmpeg are the converters the service shells out to; tests need them in the container.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libvips-tools ffmpeg \
    && rm -rf /var/lib/apt/lists/*
