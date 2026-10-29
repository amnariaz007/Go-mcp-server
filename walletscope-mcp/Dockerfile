# syntax=docker/dockerfile:1

# ---- build stage --------------------------------------------------------------
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/amnariaz007/walletscope-mcp/internal/mcpserver.Version=${VERSION}" \
      -o /out/walletscope ./cmd/walletscope

# ---- runtime stage ------------------------------------------------------------
# distroless/static: CA certificates and tzdata, no shell, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/walletscope /walletscope
USER nonroot:nonroot
# Default to the HTTP transport inside containers; override with
# `docker run -i ... walletscope-mcp -transport stdio` for stdio clients.
ENV WALLETSCOPE_TRANSPORT=http \
    WALLETSCOPE_HTTP_ADDR=0.0.0.0:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/walletscope", "healthcheck", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["/walletscope"]
