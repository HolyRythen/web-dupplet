#!/bin/bash
# Web-Dupplet – Installation als systemd-Dienst (Debian/Ubuntu/RHEL/…)
# Aufruf:  sudo ./install.sh            (Port 8080)
#          sudo PORT=9000 ./install.sh  (anderer Port)
set -euo pipefail
cd "$(dirname "$0")"

[ "$(id -u)" = 0 ] || { echo "Bitte mit sudo ausführen: sudo ./install.sh"; exit 1; }

case "$(uname -m)" in
  x86_64|amd64)  BIN=web-dupplet-linux-amd64 ;;
  aarch64|arm64) BIN=web-dupplet-linux-arm64 ;;
  *) echo "Nicht unterstützte Architektur: $(uname -m)"; exit 1 ;;
esac
[ -f "$BIN" ] || { echo "Datei $BIN fehlt im Paket"; exit 1; }

PORT="${PORT:-8080}"
APP=/opt/web-dupplet
DATA=/var/lib/web-dupplet
ENVF=/etc/web-dupplet.env

echo "==> Benutzer 'dupplet' anlegen (ohne Login)"
id dupplet >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin dupplet

echo "==> Programm nach $APP kopieren"
install -d -m 755 "$APP"
systemctl stop web-dupplet 2>/dev/null || true
install -m 755 "$BIN" "$APP/web-dupplet"
install -m 644 README.md "$APP/README.md" 2>/dev/null || true
install -m 755 quellen-demo.sh "$APP/quellen-demo.sh"
install -d -m 750 -o dupplet -g dupplet "$DATA" "$DATA/store"

echo "==> rsync prüfen (für rsync://-Quellen)"
if ! command -v rsync >/dev/null; then
  if command -v apt-get >/dev/null; then apt-get install -y -q rsync >/dev/null
  elif command -v dnf >/dev/null; then dnf install -y -q rsync
  elif command -v yum >/dev/null; then yum install -y -q rsync
  else echo "   Hinweis: rsync bitte manuell installieren"; fi
fi

if [ ! -f "$ENVF" ]; then
  echo "==> Konfiguration $ENVF anlegen (mit zufälligem Passwort)"
  PW=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 16)
  cat > "$ENVF" <<CONF
# Web-Dupplet Einstellungen – nach Änderung: sudo systemctl restart web-dupplet
LISTEN=0.0.0.0:$PORT
AUTH=admin:$PW
# Abgestimmt auf 1 Gbit/s und mehrere Kerne:
WORKERS=-fetch-workers 256 -probe-workers 128 -per-host 24 -probe-per-host 16 -local-workers 8
# Weitere Optionen, z.B. Dateien behalten:  EXTRA=-store $DATA/store
EXTRA=
CONF
  chmod 600 "$ENVF"
else
  echo "==> Bestehende Konfiguration $ENVF bleibt erhalten"
fi

echo "==> systemd-Dienst einrichten"
cat > /etc/systemd/system/web-dupplet.service <<UNIT
[Unit]
Description=Web-Dupplet – Duplikat-Finder
After=network-online.target
Wants=network-online.target

[Service]
User=dupplet
Group=dupplet
EnvironmentFile=$ENVF
WorkingDirectory=$DATA
ExecStart=$APP/web-dupplet -db $DATA/dupplet.db -listen \${LISTEN} -auth \${AUTH} \$WORKERS \$EXTRA
Restart=on-failure
RestartSec=5
KillSignal=SIGINT
TimeoutStopSec=30
LimitNOFILE=1048576
# Ressourcen: max. ~85 % der CPU-Kerne
CPUQuota=$(( $(nproc) * 85 ))%
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DATA

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now web-dupplet

if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
  echo "==> Firewall (ufw): Port $PORT freigeben"; ufw allow "$PORT"/tcp >/dev/null
elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
  echo "==> Firewall (firewalld): Port $PORT freigeben"; firewall-cmd -q --permanent --add-port="$PORT"/tcp; firewall-cmd -q --reload
fi

sleep 2
IP=$(hostname -I 2>/dev/null | awk '{print $1}')
LISTEN_PORT=$(grep '^LISTEN=' "$ENVF" | sed 's/.*://')
CRED=$(grep '^AUTH=' "$ENVF" | cut -d= -f2)
echo
if systemctl is-active --quiet web-dupplet; then
  echo "✅ Web-Dupplet läuft."
else
  echo "❌ Start fehlgeschlagen – Log: journalctl -u web-dupplet -n 50"; exit 1
fi
echo "   Web-UI:    http://$IP:$LISTEN_PORT"
echo "   Benutzer:  ${CRED%%:*}"
echo "   Passwort:  ${CRED#*:}"
echo
echo "   Test-Quellen starten:  $APP/quellen-demo.sh"
echo "   Log ansehen:           journalctl -u web-dupplet -f"
echo "   Stoppen/Starten:       sudo systemctl stop|start web-dupplet"
echo "   Einstellungen:         sudo nano $ENVF"
