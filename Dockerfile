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

# The Caddy version is pinned so the image cannot drift away from what the
# tests run against. Dependency versions are recorded in /out/versions.txt,
# which ships in the image: compare it with go.mod when something behaves
# differently in the container than it does under `go test`.
ARG CADDY_VERSION=v2.11.9

RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    xcaddy build "${CADDY_VERSION}" \
    --with github.com/frinknet/caddy-sqlite \
    --replace github.com/frinknet/caddy-sqlite=/src/caddy-sqlite \
    --output /out/caddy

# Collect licenses and the resolved version list from the *uncompressed*
# binary: upx strips the Go build info, so this must run before compression.
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod \
    sh -euxc ' \
      GOMODCACHE=$(go env GOMODCACHE); \
      mkdir -p /out/licenses/caddy /out/licenses/plugins; \
      go version -m /out/caddy > /out/versions.txt; \
      for f in "$GOMODCACHE"/github.com/caddyserver/caddy/v2@*/LICENSE*; do \
        if [ -e "$f" ]; then cp "$f" /out/licenses/caddy/; fi; \
      done; \
      go version -m /out/caddy | awk '"'"'$1 == "dep" {print $2 "@" $3}'"'"' | while read -r modver; do \
        mod="${modver%@*}"; \
        escaped=$(printf "%s" "$modver" | sed '"'"'s/[A-Z]/!\L&/g'"'"'); \
        dir="$GOMODCACHE/$escaped"; \
        if [ -d "$dir" ]; then \
          pkg=$(printf "%s" "$mod" | tr / _); \
          dest="/out/licenses/plugins/$pkg"; \
          mkdir -p "$dest"; \
          for lf in "$dir"/LICENSE* "$dir"/NOTICE*; do \
            if [ -e "$lf" ]; then cp "$lf" "$dest/"; fi; \
          done; \
        fi; \
      done; \
      ls /out/licenses/plugins | wc -l'

RUN upx --best --lzma /out/caddy

# ---------------------------------------------------------------------------
# Stage C: Compose scratch image
# ---------------------------------------------------------------------------
FROM scratch

COPY --from=build /out/licenses             /usr/share/licenses
COPY --from=build /out/versions.txt          /usr/share/licenses/versions.txt
COPY --from=build /etc/ssl/certs           /etc/ssl/certs
COPY --from=build /usr/share/zoneinfo      /usr/share/zoneinfo
COPY --from=build /out/caddy               /bin/caddy
COPY --from=build /out/toybox              /bin/toybox
COPY             www                       /srv/www

RUN ["/bin/toybox", "sh", "-c", "cd /bin && for i in $(./toybox); do if [ \"$i\" != toybox ]; then ./toybox ln -s toybox \"$i\"; fi; done && test -L /bin/sh && test -L /bin/ls"]
RUN mkdir -p /etc/caddy /var/lib/caddy /var/cache/caddy /tmp && chmod 1777 /tmp
RUN mv /srv/www/Caddyfile /etc/caddy/Caddyfile \
 && mv /srv/www/rest.caddyfile /etc/caddy/rest.caddyfile

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
