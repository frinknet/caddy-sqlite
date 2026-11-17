# ---------------------------------------------------------------------------
# Stage A: Aquire toybox, SSL certs, tzdata. No source dependency.
# ---------------------------------------------------------------------------
FROM caddy:2.11.9-builder-alpine AS build

RUN apk add --no-cache ca-certificates tzdata curl upx

ARG TARGETARCH
RUN case "${TARGETARCH}" in \
      "amd64") TOYBOX_ARCH="x86_64" ;; \
      "arm64") TOYBOX_ARCH="aarch64" ;; \
      *) echo "Unsupported arch: ${TARGETARCH}" && exit 1 ;; \
    esac \
    && mkdir -p /out/licenses/caddy /out/licenses/toybox \
    && curl -fsSL "https://codeberg.org/landley/toybox/raw/branch/master/LICENSE" -o /out/licenses/toybox/LICENSE \
    && curl -fsSL "https://landley.net/toybox/bin/toybox-${TOYBOX_ARCH}" -o /out/toybox \
    && chmod +x /out/toybox \
    && upx --best --lzma /out/toybox

# ---------------------------------------------------------------------------
# Stage B: Build caddy binary. Source-dependent, uses shared go cache.
# ---------------------------------------------------------------------------
WORKDIR /build
COPY . /src/caddy-sqlite

RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    xcaddy build \
    --with github.com/frinknet/caddy-sqlite \
    --replace github.com/frinknet/caddy-sqlite=/src/caddy-sqlite \
    --output /out/caddy \
    && upx --best --lzma /out/caddy

RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    GOMODCACHE=$(go env GOMODCACHE) && \
    mkdir -p /out/licenses/caddy /out/licenses/plugins && \
    for f in "$GOMODCACHE"/github.com/caddyserver/caddy/v2@*/LICENSE*; do \
        [ -e "$f" ] && cp "$f" /out/licenses/caddy/; \
    done && \
    go version -m /out/caddy | awk '$1 == "dep" {print $2 "@" $3}' | while read -r modver; do \
        mod="${modver%@*}"; \
        escaped=$(echo "$modver" | sed 's/[A-Z]/!\L&/g'); \
        dir="$GOMODCACHE/$escaped"; \
        if [ -d "$dir" ]; then \
            pkg=$(echo "$mod" | tr / _); \
            dest="/out/licenses/plugins/$pkg"; \
            mkdir -p "$dest"; \
            for lf in "$dir"/LICENSE* "$dir"/NOTICE*; do \
                [ -e "$lf" ] && cp "$lf" "$dest/"; \
            done; \
        fi; \
    done

# ---------------------------------------------------------------------------
# Stage C: Compose scratch image
# ---------------------------------------------------------------------------
FROM scratch

COPY --from=build /out/licenses             /usr/share/licenses
COPY --from=build /etc/ssl/certs           /etc/ssl/certs
COPY --from=build /usr/share/zoneinfo      /usr/share/zoneinfo
COPY --from=build /out/caddy               /bin/caddy
COPY --from=build /out/toybox              /bin/toybox
COPY             www                       /srv/www

RUN ["/bin/toybox", "sh", "-c", "cd /bin && for i in $(./toybox); do if [ \"$i\" != toybox ]; then ./toybox ln -s toybox \"$i\"; fi; done && test -L /bin/sh && test -L /bin/ls"]
RUN mkdir -p /etc/caddy /var/lib/caddy /var/cache/caddy /tmp && chmod 1777 /tmp
RUN mv /srv/www/Caddyfile /etc/caddy/Caddyfile

VOLUME ["/var/lib/caddy"]

ENV PATH="/bin:/usr/bin:/usr/local/bin"
ENV XDG_CONFIG_HOME="/etc/caddy"
ENV XDG_DATA_HOME="/var/lib/caddy"
ENV XDG_CACHE_HOME="/var/cache/caddy"
ENV TMPDIR="/tmp"
ENV HOME="/tmp"

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD /bin/toybox wget -O /dev/null -T 2 http://127.0.0.1:2019/config/ >/dev/null 2>&1

EXPOSE 80 443 443/udp

WORKDIR /srv/www
ENTRYPOINT ["/bin/caddy"]
CMD ["run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]
