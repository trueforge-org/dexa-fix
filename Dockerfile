# Goreleaser-friendly image: the prebuilt static binary is copied in (no build stage).
# To build by hand: go build -o dexa-fix . && docker build -t dexa-fix .
FROM gcr.io/distroless/static-debian12:nonroot

COPY dexa-fix /dexa-fix

# The app runs as the nonroot user (uid/gid 65532). The /data volume must be writable by that
# user: with a bind mount run `chown 65532:65532 <dir>`; named volumes copy the image ownership.
WORKDIR /data
VOLUME /data

ENV DEXAFIX_LISTEN=:8080 \
    DEXAFIX_DSN=file:/data/dexa-fix.db \
    DEXAFIX_ENCRYPTION_KEY_FILE=/data/dexa-fix.key

EXPOSE 8080
ENTRYPOINT ["/dexa-fix"]
CMD ["serve"]
