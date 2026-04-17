# Build stage
FROM golang:1.26-alpine AS builder

# Install build dependencies for SQLite (CGO) and Node.js for web build
RUN apk add --no-cache gcc musl-dev nodejs npm

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy web package files and install dependencies
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci

# Copy web source files and build frontend
COPY web/build.mjs ./web/
COPY web/src ./web/src
RUN cd web && npm run build

# Copy source code
COPY . .

# Build the server (CGO required for SQLite)
RUN CGO_ENABLED=1 GOOS=linux go build -o server ./cmd/server

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates sqlite

WORKDIR /app

COPY --from=builder /app/server .
COPY --from=builder /app/web/app.bundle.js /app/web/app.bundle.js.map ./web/
COPY --from=builder /app/web/*.html ./web/

EXPOSE 8080

CMD ["./server", "serve"]