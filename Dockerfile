FROM golang:1.27-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o open-lobby-relay .

FROM alpine:3.24
RUN apk --no-cache add ca-certificates wget
RUN adduser -D -u 1000 relay
WORKDIR /app
COPY --from=builder --chown=1000:1000 /app/open-lobby-relay .
EXPOSE 8080
EXPOSE 10000-10099
USER 1000
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- localhost:8080/health || exit 1
CMD ["./open-lobby-relay"]
