#!/usr/bin/env bash
set -euo pipefail

ext=so
case "$(uname -s)" in
  Darwin) ext=dylib ;;
  MINGW*|MSYS*|CYGWIN*) ext=dll ;;
esac

go mod tidy
go build -buildmode=c-shared -o "api-balance.${ext}" .
rm -f api-balance.h
echo "built api-balance.${ext}"
