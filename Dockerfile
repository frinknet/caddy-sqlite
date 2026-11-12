# Stage 1: Build static xcaddy binary
FROM caddy:2-builder-alpine AS builder

WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux xcaddy build \
    --with github.com/abiosoft/caddy-exec \
    --with github.com/git001/caddyv2-upload \
    --with caddysqlite=. \
    --output /out/caddy

# Stage 2: Grab static Toybox + root certs + tzdata from Alpine
FROM alpine:3.21 AS assets

RUN apk add --no-cache ca-certificates tzdata curl

ARG TARGETARCH
RUN case "${TARGETARCH}" in \
      "amd64") TOYBOX_ARCH="x86_64" ;; \
      "arm64") TOYBOX_ARCH="aarch64" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}" && exit 1 ;; \
    esac && \
    mkdir -p /out/bin /out/data /out/config && \
    curl -fsSL "https://landley.net/toybox/bin/toybox-${TOYBOX_ARCH}" -o /out/toybox && \
    chmod +x /out/toybox && \
    cp /out/toybox /out/bin/toybox && \
    for cmd in sh ls ps cp mv rm mkdir cat grep tar find df du; do \
      ln -s /bin/toybox "/out/bin/${cmd}"; \
    done

# Stage 3: Final ~37MB scratch image
FROM scratch

COPY --from=assets /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=assets /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=assets /out/bin /bin
COPY --from=assets /out/data /data
COPY --from=assets /out/config /config

COPY --from=builder /out/caddy /bin/caddy
COPY Caddyfile /etc/caddy/Caddyfile

ENV PATH="/bin"
ENV XDG_DATA_HOME="/data"
ENV XDG_CONFIG_HOME="/config"

VOLUME ["/data", "/config"]
EXPOSE 80 443 443/udp

ENTRYPOINT ["/bin/caddy"]
CMD ["run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
