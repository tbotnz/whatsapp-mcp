# Deploying on a host that already runs a reverse proxy

When another compose stack's nginx container owns host ports 80 and 443, this
stack cannot publish its own TLS endpoint, and a containerised proxy cannot
reach a port published on the host's loopback. With the
`docker-compose.vps.yml` overlay this stack publishes no host ports: the MCP
server joins the proxy's Docker network as `whatsapp-mcp`, and the proxy
forwards a dedicated hostname to it. The bridge's port 8080 is
container-internal, so it cannot clash with anything on the host or in the
other stack.

Use a dedicated hostname rather than a path under an existing site: the
server needs `/mcp`, `/.well-known/oauth-*` and the OAuth endpoints at the
root of its hostname.

## 1. DNS

Point an A record for the hostname (below: `whatsapp.example.com`) at the host.

## 2. This stack

Find the proxy's network with `docker network ls` (for a compose project it
is usually `<project>_default`), then:

```sh
git clone <this repo> ~/whatsapp-mcp && cd ~/whatsapp-mcp
cp .env.example .env
# in .env:
#   WHATSAPP_MCP_PUBLIC_URL=https://whatsapp.example.com
#   COMPOSE_FILE=docker-compose.yml:docker-compose.vps.yml
#   WHATSAPP_MCP_EDGE_NETWORK=<proxy network>
docker compose up -d --build
docker compose logs -f bridge     # scan the QR code
```

`docker compose ps` should show no published ports.

## 3. Certificate

Issue a certificate for the hostname with whatever the proxy stack uses. With
a certbot container and a webroot, for example (run in the proxy stack's
directory; a separate `--cert-name` leaves existing certificates untouched):

```sh
docker compose run --rm --entrypoint certbot certbot certonly \
  --webroot -w /var/www/certbot \
  --cert-name whatsapp.example.com -d whatsapp.example.com \
  --email <you> --agree-tos --no-eff-email
```

The HTTP-01 challenge only works if the proxy already serves
`/.well-known/acme-challenge/` for this hostname on port 80.

## 4. nginx

Copy `whatsapp-mcp.conf` from this directory into the proxy's configuration,
replace the hostname and adjust paths, and mount it into the nginx container:

```yaml
      - ./whatsapp-mcp.conf:/etc/nginx/conf.d/whatsapp-mcp.conf:ro
```

Then check and reload:

```sh
docker compose exec <nginx service> nginx -t
docker compose exec <nginx service> nginx -s reload
curl -fsS https://whatsapp.example.com/health
```

Do step 3 before step 4: nginx will not start with a missing certificate.

## 5. claude.ai

Add a custom connector with URL `https://whatsapp.example.com/mcp`. The
approval code appears in `docker compose logs mcp` in this stack.

## Notes

- Containers on the shared network can reach the MCP server on port 8000 and
  it can reach them. Its endpoints are all OAuth-protected, and the WhatsApp
  bridge itself is not on that network. For stricter separation, create a
  dedicated network that only the proxy and the MCP server join.
- The `cloudflared` profile is an alternative that needs no nginx or
  certificate changes, but the hostname's DNS must be on Cloudflare.
