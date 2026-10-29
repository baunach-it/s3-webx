FROM golang:1.26.8-alpine3.24 AS build

# The converter tests shell out to vips and ffmpeg; a skipped test would hide a broken converter.
RUN apk add --no-cache vips-tools ffmpeg

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY config.example.yaml ./

RUN go vet ./...
RUN go test -count=1 -v ./... > /tmp/test.log 2>&1; status=$?; \
    grep -E '^(ok|FAIL|--- (FAIL|SKIP)|panic)' /tmp/test.log; \
    if [ "$status" -ne 0 ]; then cat /tmp/test.log; exit "$status"; fi; \
    if grep -q -- '--- SKIP' /tmp/test.log; then echo "skipped tests are not allowed in the image build"; exit 1; fi

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/s3-webx ./cmd/s3-webx

# The binary redistributes every linked module, so their license texts ship with it; a module without one stops the build.
COPY LICENSE ./
RUN set -e; out=/out/licenses; mkdir -p "$out/go"; cp LICENSE "$out/"; cp "$(go env GOROOT)/LICENSE" "$out/go/"; \
    go list -deps -f '{{with .Module}}{{if .Version}}{{.Path}}@{{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/s3-webx | sort -u | \
    while read -r mod dir; do \
        mkdir -p "$out/$mod"; \
        find "$dir" -maxdepth 1 -type f \( -iname 'licen[cs]e*' -o -iname 'copying*' -o -iname 'notice*' -o -iname 'patents*' \) \
            -exec cp {} "$out/$mod/" \; ; \
        if [ -z "$(ls "$out/$mod")" ]; then echo "no license file in $mod"; exit 1; fi; \
    done

FROM alpine:3.24.1

RUN apk add --no-cache vips-tools ffmpeg ca-certificates tzdata \
    && addgroup -S -g 10001 s3webx \
    && adduser -S -D -H -u 10001 -G s3webx -h /data s3webx \
    && mkdir -p /data /config \
    && chown 10001:10001 /data

COPY --from=build /out/s3-webx /usr/local/bin/s3-webx
COPY --from=build /out/licenses /usr/share/licenses/s3-webx

USER 10001:10001
VOLUME /data
EXPOSE 8080

# Assumes the default listen address :8080.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/s3-webx"]
CMD ["-config", "/config/s3-webx.yaml"]
