#!/bin/sh
set -eu

mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/music-utils ./cmd/server
