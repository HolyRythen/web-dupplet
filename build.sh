#!/bin/sh
# Baut ein einzelnes, statisches Binary (keine Abhängigkeiten zur Laufzeit).
cd "$(dirname "$0")"
export PATH="$HOME/.local/go/bin:$PATH"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o web-dupplet . && echo "fertig: ./web-dupplet"
