#!/bin/sh
# Baut ein einzelnes, statisches Binary für diesen Rechner (keine Abhängigkeiten zur Laufzeit).
# Für das Installationspaket (x86_64 + ARM64): ./paket.sh
cd "$(dirname "$0")"
command -v go >/dev/null || export PATH="$HOME/.local/go/bin:$PATH"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o web-dupplet . && echo "fertig: ./web-dupplet"
