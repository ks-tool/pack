#!/usr/bin/env sh
# Regenerate the provider protocol stubs from proto/provider.proto into
# provider/providerpb — one package for both sides: pack is the client, a
# provider binary the server.
#
# Requires protoc + protoc-gen-go + protoc-gen-go-grpc on PATH.
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

pkg=provider/providerpb
mkdir -p "$pkg"
protoc -I proto \
	--go_out="$pkg" --go_opt=paths=source_relative \
	--go-grpc_out="$pkg" --go-grpc_opt=paths=source_relative \
	provider.proto
