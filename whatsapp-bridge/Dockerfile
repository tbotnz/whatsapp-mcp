# syntax=docker/dockerfile:1.7
# Go bridge: holds the WhatsApp session and serves the internal REST API.
# Built with CGO because go-sqlite3 (whatsmeow's session store) needs it.
FROM golang:1.27-bookworm AS build
WORKDIR /src
ENV CGO_ENABLED=1 GOFLAGS=-trimpath GOTOOLCHAIN=auto
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/whatsapp-bridge .

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 1000 app && useradd -u 1000 -g app -m -s /usr/sbin/nologin app \
 && mkdir -p /data/store && chown -R app:app /data && chmod 700 /data/store
COPY --from=build /out/whatsapp-bridge /usr/local/bin/whatsapp-bridge
USER app
# The bridge keeps everything under ./store relative to its working directory.
WORKDIR /data
VOLUME ["/data/store"]
# Inside the compose network the MCP container talks to us by service name,
# so bind on all container interfaces. The port is NOT published to the host.
ENV WHATSAPP_BRIDGE_ADDR=0.0.0.0:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8080/health >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/whatsapp-bridge"]
