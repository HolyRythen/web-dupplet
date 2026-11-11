#!/bin/sh
# Baut das Installationspaket dist/web-dupplet.tar.gz (x86_64 + ARM64, statisch).
set -e
cd "$(dirname "$0")"
command -v go >/dev/null || export PATH="$HOME/.local/go/bin:$PATH"
D=dist/web-dupplet
rm -rf dist && mkdir -p "$D"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags="-s -w" -o "$D/web-dupplet-linux-$arch" .
done
cp README.md deploy/install.sh deploy/uninstall.sh deploy/quellen-demo.sh deploy/ANLEITUNG.txt "$D/"
chmod 755 "$D"/*.sh "$D"/web-dupplet-linux-*
tar -C dist -czf dist/web-dupplet.tar.gz web-dupplet
(cd dist && sha256sum web-dupplet.tar.gz > web-dupplet.tar.gz.sha256)
rm -rf "$D"
echo "fertig: dist/web-dupplet.tar.gz ($(du -h dist/web-dupplet.tar.gz | cut -f1))"
