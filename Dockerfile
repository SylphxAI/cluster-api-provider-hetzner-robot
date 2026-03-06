# Build stage — Go 1.25 required by siderolabs/talos/pkg/machinery
FROM golang:1.25 AS builder

WORKDIR /workspace

# Cache dependencies
COPY go.mod go.sum ./
RUN GOFLAGS=-mod=mod GONOSUMDB=* go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    GOFLAGS=-mod=mod GONOSUMDB=* \
    go build -ldflags="-w -s" -o manager ./main.go

# Runtime stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/manager .

USER 65532:65532

ENTRYPOINT ["/manager"]
