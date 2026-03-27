# Build stage
FROM golang:1.24-alpine AS builder
WORKDIR /app

# Download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o exchange_server ./cmd/exchange/main.go

# Production stage
FROM alpine:latest
WORKDIR /root/
RUN apk add --no-cache ca-certificates

# Copy the binary and UI assets from builder
COPY --from=builder /app/exchange_server .
COPY --from=builder /app/public ./public

EXPOSE 8080
CMD ["./exchange_server"]
