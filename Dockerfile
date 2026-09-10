FROM golang:1.26 AS builder

WORKDIR /build
COPY . /build

RUN CGO_ENABLED=0 go build -ldflags '-s -w -extldflags "-static"' -o firmirror ./cmd/firmirror.go

FROM debian:stable-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates fwupd jcat \
 && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 1000 -s /bin/bash firmirror \
 && mkdir /data && chown firmirror:firmirror /data

WORKDIR /data

COPY --from=builder /build/firmirror /bin/firmirror
RUN chmod +x /bin/firmirror

USER firmirror

ENTRYPOINT ["/bin/firmirror"]
