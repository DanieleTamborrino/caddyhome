FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod main.go index.html ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /caddyhome .

FROM alpine:3.20
RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /caddyhome /usr/local/bin/caddyhome
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/caddyhome"]
