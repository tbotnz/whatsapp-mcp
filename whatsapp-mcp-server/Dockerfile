# syntax=docker/dockerfile:1.7
# Python MCP server. In the container it runs in HTTP mode behind the built-in
# OAuth server; set the WHATSAPP_MCP_* variables in the compose .env file.
FROM python:3.12-slim-bookworm
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_NO_CACHE=1 PYTHONUNBUFFERED=1
RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir --root-user-action=ignore "uv>=0.13,<0.14" \
 && groupadd -g 1000 app && useradd -u 1000 -g app -m -s /usr/sbin/nologin app \
 && mkdir -p /data/store /app && chown -R app:app /data /app
WORKDIR /app
COPY --chown=app:app pyproject.toml uv.lock ./
RUN --mount=type=cache,target=/root/.cache/uv uv sync --locked --no-dev --no-install-project
COPY --chown=app:app *.py ./
USER app
ENV PATH="/app/.venv/bin:$PATH" \
    WHATSAPP_BRIDGE_STORE=/data/store \
    WHATSAPP_API_BASE_URL=http://bridge:8080/api \
    WHATSAPP_MCP_TRANSPORT=http \
    WHATSAPP_MCP_HOST=0.0.0.0 \
    WHATSAPP_MCP_PORT=8000 \
    WHATSAPP_MCP_EXTRA_ALLOWED_HOSTS=mcp:8000
VOLUME ["/data/store"]
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/health', timeout=3).status==200 else 1)"
CMD ["python", "main.py"]
