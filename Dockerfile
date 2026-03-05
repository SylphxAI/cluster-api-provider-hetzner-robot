# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /workspace

# Install talosctl
ARG TALOS_VERSION=v1.12.4
RUN apk add --no-cache curl && \
    curl -fsSL "https://github.com/siderolabs/talos/releases/download/${TALOS_VERSION}/talosctl-linux-amd64" \
    -o /usr/local/bin/talosctl && \
    chmod +x /usr/local/bin/talosctl

# Cache dependencies using go.sum for reproducible builds
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o manager ./main.go

# Runtime stage
FROM alpine:3.21

WORKDIR /

RUN apk add --no-cache ca-certificates openssh-client curl

COPY --from=builder /usr/local/bin/talosctl /usr/local/bin/talosctl
COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
