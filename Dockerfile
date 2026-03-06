# Build stage
FROM golang:1.26-alpine AS builder

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

# SSH operations use Go's golang.org/x/crypto/ssh (no system binary needed)
RUN apk add --no-cache ca-certificates

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
