# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o inkframe-backend ./cmd/server

# Runtime stage
FROM alpine:3.20

WORKDIR /app

RUN apk --no-cache add ca-certificates tzdata && \
    adduser -D -g '' appuser

COPY --from=builder /app/inkframe-backend .
COPY --from=builder /app/config.example.yaml ./config.yaml

RUN mkdir -p uploads && chown -R appuser:appuser /app

USER appuser

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

CMD ["./inkframe-backend"]
