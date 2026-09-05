# Build stage
FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o agent ./cmd/agent

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /app/agent .

# The pinned FRP client (relay/frp/manifest.json) must be staged into this
# build context as `frpc` before building: the release workflow copies it
# there only after relay/scripts/fetch-frp.sh verified its SHA-256 against
# the pinned digest, and that verification fails the build on mismatch.
# The binary is what agent/internal/tunnel supervises; its default path is
# next to the agent executable.
COPY frpc /app/frpc

EXPOSE 7878

CMD ["./agent", "daemon"]
