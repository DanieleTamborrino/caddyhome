FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod main.go index.html logo.svg ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /caddyhome .

FROM alpine:3.20
RUN apk --no-cache add ca-certificates tzdata su-exec \
    && addgroup -g 1000 caddyhome \
    && adduser -D -u 1000 -G caddyhome caddyhome \
    && mkdir -p /data \
    && chown -R 1000:1000 /data
COPY --from=builder /caddyhome /usr/local/bin/caddyhome
ENV PUID=1000 \
    PGID=1000
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["sh", "-c", "DATA_DIR=$(dirname \"${CONFIG_PATH:-/data/config.json}\") && mkdir -p \"$DATA_DIR\" && chown -R ${PUID:-1000}:${PGID:-1000} \"$DATA_DIR\" && exec su-exec ${PUID:-1000}:${PGID:-1000} /usr/local/bin/caddyhome"]
