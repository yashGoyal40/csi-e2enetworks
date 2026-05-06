# syntax=docker/dockerfile:1.7

# ----- builder ----------------------------------------------------------------
FROM golang:1.22-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/csi-e2enetworks ./cmd/csi-e2enetworks

# ----- runtime ----------------------------------------------------------------
# We need a few host binaries for the Node plugin: blkid, mkfs.ext4, mount,
# umount, and pciutils for triggering rescans. Alpine ships compatible builds.
FROM alpine:3.20
RUN apk add --no-cache \
      ca-certificates \
      e2fsprogs \
      e2fsprogs-extra \
      xfsprogs \
      util-linux \
      blkid \
      pciutils \
      bash
COPY --from=builder /out/csi-e2enetworks /usr/local/bin/csi-e2enetworks
ENTRYPOINT ["/usr/local/bin/csi-e2enetworks"]
