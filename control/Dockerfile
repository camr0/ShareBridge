# Build stage
FROM golang:1.26-alpine AS builder

# Install build dependencies for SQLite (CGO)
RUN apk add --no-cache gcc musl-dev

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the server (CGO required for SQLite)
RUN CGO_ENABLED=1 GOOS=linux go build -o server ./cmd/server

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates sqlite

WORKDIR /app

COPY --from=builder /app/server .
COPY --from=builder /app/web ./web

EXPOSE 8080

CMD ["./server", "serve"]