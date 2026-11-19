#!/bin/sh

timeout 280 docker run --rm \
  -v caddy-sf-mod:/mod \
  -v "$PWD":/src \
  -w /src \
  -e GOMODCACHE=/mod \
  -e GOCACHE=/mod/gocache \
  -e CGO_ENABLED=0 caddy:2.11.9-builder-alpine \
  sh -c 'gofmt -l . && go vet   ./... && go test -timeout=120s ./...'
