# Running behind a reverse proxy

dexa-fix speaks plain HTTP. For HTTPS, put a reverse proxy such as nginx, Caddy
or Traefik in front of it, and bind dexa-fix to a private address so that
nobody can bypass the proxy: `--listen 127.0.0.1:8080`. dexa-fix can also serve
HTTPS itself, see [HTTPS, HTTP/2 and HTTP/3](#https-http2-and-http3).

A proxy hides two things from the app: who the visitor is (every connection
comes from the proxy) and whether they used HTTPS. The proxy passes both on in
the `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host` headers. Any
visitor can send those headers too, so dexa-fix only believes them on
connections from the trusted proxies. From everyone else they are ignored, and
a warning is logged once.

## Trusted proxies and base URL

Both are set on the Admin page, under **Network and retention**. They are stored
in the database, not in flags, and take effect at once without a restart.

| Setting             | What it does                                                                                                                              |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| Trusted proxies     | Comma-separated addresses or CIDR ranges of your proxies, such as `127.0.0.1,10.0.0.0/8`. The keyword `loopback` stands for `127.0.0.0/8` and `::1`; `private` adds the RFC 1918 ranges and `fc00::/7`. Empty trusts nobody. |
| Base URL            | The public address, scheme and host only (no path), such as `https://dexa.example.com`. Optional when the proxy forwards `Host` and `X-Forwarded-Proto` correctly; set it for a fixed, predictable address. Saving it is accepted only while you use that address, so a typo cannot lock you out. |

What changes when the proxy is trusted:

- **Sign-in limits and the audit log** use the visitor's address. Without this,
  everyone shares the proxy's address: one person's failed sign-ins would block
  everybody, and the audit log would show only the proxy.
- **Cookies are marked `Secure`** and `Strict-Transport-Security` is sent, as
  soon as the visitor used HTTPS (as reported by the proxy), or the base URL is
  `https`.
- **Sign-in provider callback URLs** (the ones to register at your identity
  provider, shown on the Admin page) are built from the base URL, or else from
  `X-Forwarded-Proto` and the host.
- **Form posts pass the same-origin check.** The browser's `Origin` has to match
  the host it visited, which is why the proxy must pass the original host, either
  as `Host` or as `X-Forwarded-Host`, or you set the base URL.

For the address, the app reads `X-Forwarded-For` from the right: it skips every
address that belongs to a trusted proxy, and the first address after that is
the visitor. Anything the visitor wrote further left is never used. Make sure
the proxy appends to this header instead of passing along what the client sent.
Without `X-Forwarded-For`, `X-Real-IP` is used.

## Examples

Examples, assuming dexa-fix listens on `127.0.0.1:8080` on the same machine:

```text
# nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}

# Caddy: forwards the headers by default
dexa.example.com {
    reverse_proxy 127.0.0.1:8080
}

# Traefik (docker labels)
traefik.http.routers.dexa.rule=Host(`dexa.example.com`)
traefik.http.routers.dexa.tls=true
traefik.http.services.dexa.loadbalancer.server.port=8080
```

On the Admin page, set **Trusted proxies** to `loopback` and **Base URL** to
`https://dexa.example.com`, then start dexa-fix with:

```sh
dexa-fix serve --listen 127.0.0.1:8080
```

In Docker, the proxy reaches the app from the network's bridge address, so use
the network's range, or `private`. Only trust ranges that nothing but your
proxies can send from.

dexa-fix must be served from the root of its host. A sub-path such as
`example.com/dexa/` is not supported. If you use Withings, the redirect URL on the Admin page
has to be the public callback address, for example
`https://dexa.example.com/integrations/withings/callback`.

## PROXY protocol

The PROXY protocol (HAProxy versions 1 and 2) passes the visitor's address on
the TCP connection itself, before any HTTP. Use it with a balancer that works at
TCP level and adds no `X-Forwarded-For` header.

Turn on **Accept the PROXY protocol from the trusted proxies** on the Admin page.
It needs trusted proxies. With it on:

- A connection from a trusted proxy may start with the header. The address in it
  becomes the visitor's address. The header is optional there, so a balancer
  that does not send it cannot lock anyone out.
- A connection from any other address may not start with the header. It is
  refused, so nobody can claim an address.
- With the setting off, connections pass through untouched.

It applies to the plain and the HTTPS listener, and to each new connection at
once. The header does not say whether the visitor used HTTPS, so set an `https`
base URL to get `Secure` cookies.

HAProxy terminates TLS and sends the address with `send-proxy-v2`:

```text
# haproxy.cfg
frontend dexa
    mode http
    bind :443 ssl crt /etc/haproxy/dexa.pem
    http-request set-header X-Forwarded-Proto https
    default_backend dexa

backend dexa
    mode http
    server dexa 127.0.0.1:8080 send-proxy-v2
```

An AWS Network Load Balancer works at TCP level and adds no headers. Use a TCP
listener on port 443, forward it to a target group on port 8443 (the HTTPS port
of dexa-fix, with a certificate you upload on the Admin page), and turn on
**Proxy protocol v2** for the target group. Trusted proxies must cover the
addresses the load balancer connects from, for example your VPC range.

nginx passes TCP through with the `stream` module and sends the address with
`proxy_protocol on`:

```text
# nginx.conf, stream context
stream {
    server {
        listen 443;
        proxy_pass 127.0.0.1:8443;
        proxy_protocol on;
    }
}
```

Here dexa-fix serves HTTPS on port 8443 and trusts `loopback`.

## HTTPS, HTTP/2 and HTTP/3

dexa-fix can serve HTTPS itself. Behind a reverse proxy you usually leave this
off and let the proxy terminate TLS. Plain HTTP stays on the `--listen` port,
with HTTP/1.1 and cleartext HTTP/2 (h2c, with prior knowledge) for proxies that
speak it to the backend.

Set it on the Admin page, under **HTTPS, HTTP/2 and HTTP/3**:

- **Certificate**: either upload a PEM certificate chain and a PEM private key,
  or let dexa-fix generate a self-signed certificate. An uploaded key is stored
  encrypted and is never shown again. Leave both fields empty to keep the saved
  certificate.
- A **self-signed** certificate is ECDSA P-256, valid 365 days, and renewed
  automatically 30 days before it expires. Its names are the names you give, or
  else the base URL's host plus `localhost`, `127.0.0.1` and `::1`.
- **HTTPS port**: 8443 by default. It must differ from the plain HTTP port. HTTPS
  runs on its own listener, so a bad certificate or port cannot lock you out of
  the plain one. HTTP/2 and HTTP/1.1 are offered by ALPN on TLS 1.2 and later.
- **Also serve HTTP/3 (QUIC)**: runs on the UDP port with the same number as the
  HTTPS port, and is advertised to HTTPS clients with an `Alt-Svc` header. The
  UDP port must be reachable.

The Admin page shows the certificate names, expiry, SHA-256 fingerprint and any
bind error.

With Docker, publish both ports, and the UDP one for HTTP/3:

```bash
docker run -p 8080:8080 -p 8443:8443 -p 8443:8443/udp -v dexa-fix-data:/data ghcr.io/trueforge-org/dexa-fix:latest
```

Browsers and apps trust a self-signed certificate only once you add it to their
trust store, and browsers use HTTP/3 only with a trusted certificate. There is
no redirect from HTTP to HTTPS yet.
