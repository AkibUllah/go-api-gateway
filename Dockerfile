# syntax=docker/dockerfile:1.7

# ---- build ----------------------------------------------------------------
FROM golang:1.27-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X github.com/AkibUllah/go-api-gateway/internal/app.Version=${VERSION}" \
      -o /out/ ./cmd/gateway ./cmd/gateway-demo ./cmd/echo-upstream ./cmd/devtoken

# ---- runtime: static binaries on distroless, no shell, non-root ------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
COPY configs/gateway.yaml /etc/gateway/gateway.yaml
ENV GATEWAY_CONFIG=/etc/gateway/gateway.yaml
USER nonroot:nonroot
EXPOSE 8080 9090

# No curl in distroless: the gateway binary probes itself.
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s \
  CMD ["/usr/local/bin/gateway", "-healthcheck", "http://127.0.0.1:8080/healthz"]

# CMD (not ENTRYPOINT) so compose and Render can run the other binaries
# from the same image: echo-upstream, gateway-demo.
CMD ["/usr/local/bin/gateway"]
