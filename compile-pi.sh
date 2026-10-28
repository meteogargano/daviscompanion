#!/bin/sh

VERSION=$(git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS="-s -w -X main.version=$VERSION"

mkdir -p dist

GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o dist/davisweather-armv7l ./...
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$LDFLAGS" -o dist/davisweather-aarch64 ./...
