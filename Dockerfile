# midea-mcp-control container image.
#
# The device inventory (long-lived LAN tokens/keys) is NEVER baked into the
# image. The showboat deployment injects it at runtime and materializes
# ~/.config/midea-mcp-control/devices.json with mode 0600 before the server
# starts (see the midea-entrypoint.sh in the showboat stack).
#
# Build and publish (see Makefile):
#   make docker-push
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath -ldflags "-s -w" \
    -o /out/midea-mcp-control ./cmd/midea-mcp-control

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /out/midea-mcp-control /usr/local/bin/midea-mcp-control
# 8765: MCP Streamable HTTP (bearer token) — 9103: Prometheus metrics.
EXPOSE 8765 9103
ENTRYPOINT ["midea-mcp-control"]
