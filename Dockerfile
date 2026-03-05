# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /workspace

RUN apk add --no-cache git

# Cache dependencies
COPY go.mod go.sum ./
RUN GOFLAGS=-mod=mod GONOSUMDB=* go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    GOFLAGS=-mod=mod GONOSUMDB=* \
    go build -ldflags="-w -s" -o manager ./main.go

# Runtime stage
FROM alpine:3.21

WORKDIR /

# openssh-client for SSH to Hetzner rescue mode
RUN apk add --no-cache ca-certificates openssh-client

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
