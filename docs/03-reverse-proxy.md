# Running behind a reverse proxy

dexa-fix speaks plain HTTP. For HTTPS, put a reverse proxy such as nginx, Caddy
or Traefik in front of it, and bind dexa-fix to a private address so that
nobody can bypass the proxy: `--listen 127.0.0.1:8080`.

A proxy hides two things from the app: who the visitor is (every connection
comes from the proxy) and whether they used HTTPS. The proxy passes both on in
the `X-Forwarded-For`, `X-Forwarded-Proto` and `X-Forwarded-Host` headers. Any
visitor can send those headers too, so dexa-fix only believes them on
connections from the proxies you list in `--trusted-proxies`. From everyone
else they are ignored, and a warning is logged once.

| Setting             | What it does                                                                                                                              |
| ------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `--trusted-proxies` | Comma-separated addresses or CIDR ranges of your proxies, such as `127.0.0.1,10.0.0.0/8`. The keyword `loopback` stands for `127.0.0.0/8` and `::1`; `private` adds the RFC 1918 ranges and `fc00::/7`. Empty trusts nobody. |
| `--base-url`        | The public address, scheme and host only (no path). Optional when the proxy forwards `Host` and `X-Forwarded-Proto` correctly; set it for a fixed, predictable address. |

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
  as `Host` or as `X-Forwarded-Host`, or you set `--base-url`.

For the address, the app reads `X-Forwarded-For` from the right: it skips every
address that belongs to a trusted proxy, and the first address after that is
the visitor. Anything the visitor wrote further left is never used. Make sure
the proxy appends to this header instead of passing along what the client sent.
Without `X-Forwarded-For`, `X-Real-IP` is used.

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

```sh
dexa-fix serve --listen 127.0.0.1:8080 --trusted-proxies loopback --base-url https://dexa.example.com
```

In Docker, the proxy reaches the app from the network's bridge address, so use
the network's range, or `private`. Only trust ranges that nothing but your
proxies can send from.

dexa-fix must be served from the root of its host. A sub-path such as
`example.com/dexa/` is not supported. If you use Withings, the redirect URL on the Admin page
has to be the public callback address, for example
`https://dexa.example.com/integrations/withings/callback`.
