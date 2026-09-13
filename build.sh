#!/usr/bin/env bash
set -euo pipefail

ext=so
case "$(uname -s)" in
  Darwin) ext=dylib ;;
  MINGW*|MSYS*|CYGWIN*) ext=dll ;;
esac

go mod tidy
go build -buildmode=c-shared -o "third-party-balance.${ext}" .
rm -f third-party-balance.h
echo "built third-party-balance.${ext}"
