#!/usr/bin/env bash
# Build the self-contained injector/guard exe.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mkdir -p "$here/dist"

echo "==> checking the panel's syntax"
if command -v node >/dev/null 2>&1; then
  node --check "$here/injector/assets/opti-engine.js"
  echo "    ok"
else
  echo "    (node not found, skipping)"
fi

echo "==> running the tests"
(cd "$here/injector" && go test ./...)

echo "==> building the injector"
cd "$here/injector"
go build -trimpath -ldflags "-s -w" -o "$here/dist/FreebuffOpti.exe" .

echo "==> done"
ls -la "$here/dist"
