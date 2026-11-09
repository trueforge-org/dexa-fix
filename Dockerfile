# Goreleaser-friendly image: the prebuilt static binary is copied in (no Go build stage).
# To build by hand: go build -o dexa-fix . && docker build -t dexa-fix .
# The final image is `scratch`: the binary embeds its own time zones (time/tzdata) and CA roots
# (x509roots/fallback), so no OS files are needed. There is no shell: use `docker run ... <command>`
# instead of `docker exec`. Pinned by digest; the docker Dependabot ecosystem keeps it current.

# Build-only helper stage: creates the /data directory with the right owner (scratch has no mkdir/chown).
FROM --platform=$BUILDPLATFORM busybox:1.37-musl@sha256:f6a95d611e0a8780fabdb2aec43e31275b2720dfefa3e41b16835bfc42454478 AS data
RUN mkdir -p /out/data

FROM scratch

COPY dexa-fix /dexa-fix

# The app runs as uid/gid 65532. The /data volume must be writable by that user: with a bind mount
# run `chown 65532:65532 <dir>`; named volumes copy the image ownership.
COPY --from=data --chown=65532:65532 /out/data /data
USER 65532:65532
WORKDIR /data
VOLUME /data

ENV DEXAFIX_LISTEN=:8080 \
    DEXAFIX_DSN=file:/data/dexa-fix.db \
    DEXAFIX_ENCRYPTION_KEY_FILE=/data/dexa-fix.key

# 8080 is plain HTTP. HTTPS (and HTTP/3 on UDP) listen on the port set on the Admin page, 8443 by
# default, once a certificate is configured; publish it with -p 8443:8443 -p 8443:8443/udp.
EXPOSE 8080 8443 8443/udp
ENTRYPOINT ["/dexa-fix"]
CMD ["serve"]
