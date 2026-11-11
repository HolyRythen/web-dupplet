#!/bin/bash
# Startet die bewährten Testquellen (über die API des laufenden Dienstes).
# Aufruf: sudo /opt/web-dupplet/quellen-demo.sh
set -euo pipefail
ENVF=/etc/web-dupplet.env
[ -r "$ENVF" ] || { echo "Bitte mit sudo ausführen"; exit 1; }
AUTH=$(grep '^AUTH=' "$ENVF" | cut -d= -f2)
PORT=$(grep '^LISTEN=' "$ENVF" | sed 's/.*://')
add() {
  printf "  %-55s " "$1"
  curl -s -u "$AUTH" -X POST --data "{\"url\":\"$1\",\"exts\":\"${3:-*}\",\"base\":\"$2\"}" "http://127.0.0.1:$PORT/api/crawl"; echo
}
echo "Quellen werden hinzugefügt (alle Dateitypen):"
add rsync://ftp.gwdg.de/pub/                        https://ftp.gwdg.de/pub/
add rsync://ftpmirror.your.org/pub/misc/bitsavers/  https://ftpmirror.your.org/pub/misc/bitsavers/
add rsync://ftp.fau.de/ctan/                       https://ftp.fau.de/ctan/
echo "Fertig – Fortschritt in der Web-UI."
