#!/bin/bash
# Entfernt den Dienst. Datenbank/Downloads in /var/lib/web-dupplet bleiben erhalten,
# außer mit:  sudo ./uninstall.sh --alles
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "Bitte mit sudo ausführen"; exit 1; }
systemctl disable --now web-dupplet 2>/dev/null || true
rm -f /etc/systemd/system/web-dupplet.service; systemctl daemon-reload
rm -rf /opt/web-dupplet
if [ "${1:-}" = "--alles" ]; then rm -rf /var/lib/web-dupplet /etc/web-dupplet.env; userdel dupplet 2>/dev/null || true; echo "Alles entfernt."
else echo "Dienst entfernt. Daten liegen weiter in /var/lib/web-dupplet, Konfiguration in /etc/web-dupplet.env"; fi
