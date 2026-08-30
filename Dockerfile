# Build the binary against the same Go version the module targets.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first, so editing source does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=0.0.0-dev
# CGO stays off so the result is a static binary that runs on a small base.
# modernc.org/sqlite is pure Go precisely so this holds.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/serve ./cmd/serve && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/build ./cmd/build && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/seed ./cmd/seed

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 1000 -h /data readarr

# The seeder and dataset builder ship alongside the server so nobody is
# forced to depend on prebuilt datasets. Anyone with a Hardcover API token
# can build their own; see README.md.
COPY --from=build /out/serve /usr/local/bin/readarr-metadata-provider
COPY --from=build /out/build /usr/local/bin/readarr-metadata-build
COPY --from=build /out/seed /usr/local/bin/readarr-metadata-seed

# The dataset lives on a volume rather than in the image, so the data and
# the binary update independently. An empty volume is fine: the server
# downloads the published snapshot on first boot.
VOLUME /data
WORKDIR /data
USER readarr

EXPOSE 8816

# Readarr treats metadata failures as transient and retries, so an unhealthy
# container that keeps answering is worse than one that reports itself down.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8816/author/changed || exit 1

ENTRYPOINT ["readarr-metadata-provider"]
CMD ["-addr", ":8816", "-db", "/data/metadata.db"]
