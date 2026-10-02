#!/bin/sh
# Builds cyprid and copies it to Homebrew's bin folder, so `cyprid` works in any
# Terminal window. Run it again after getting a new version of the cyprid folder.
set -e
cd "$(dirname "$0")"

for tool in go uv; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "error: $tool isn't installed. Install it with: brew install go uv" >&2
    exit 1
  fi
done

echo "Building cyprid…"
go build -o bin/cyprid ./cmd/cyprid

dest="$(brew --prefix 2>/dev/null || echo /opt/homebrew)/bin"
cp bin/cyprid "$dest/cyprid"
echo "Installed $dest/cyprid"

if [ "$(command -v cyprid)" != "$dest/cyprid" ]; then
  echo "warning: 'cyprid' in this Terminal runs $(command -v cyprid || echo nothing), not $dest/cyprid." >&2
  echo "Open a new Terminal window; if that doesn't fix it, redo step 1's 'Next steps'." >&2
fi

"$dest/cyprid" version
