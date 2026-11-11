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

echo "==> Hilfsprogramme prüfen (rsync für rsync://-Quellen, ffmpeg + poppler-utils für die Ähnlichkeitsanalyse)"
need=""
command -v rsync     >/dev/null || need="$need rsync"
command -v ffmpeg    >/dev/null || need="$need ffmpeg"
command -v pdftotext >/dev/null || need="$need poppler-utils"
if [ -n "$need" ]; then
  if command -v apt-get >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends $need >/dev/null \
      || { apt-get update -q >/dev/null && DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends $need >/dev/null; }
  elif command -v dnf >/dev/null; then dnf install -y -q $need
  elif command -v yum >/dev/null; then yum install -y -q $need
  else echo "   Hinweis: bitte manuell installieren:$need"; fi
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
# Stufe 4 – ähnliche Inhalte (Bilder, Videos, Audio, Texte, PDFs, Office, Archive). Leer lassen = aus.
SIMILAR=-similar -analyze-workers 6 -media-workers 2 -sim-max 512
# Weitere Optionen, z.B. Dateien behalten:  EXTRA=-store $DATA/store
# Download-Rate begrenzen (z.B. auf geteilten Servern):  EXTRA=-max-rate 50
EXTRA=
CONF
  chmod 600 "$ENVF"
else
  echo "==> Bestehende Konfiguration $ENVF bleibt erhalten"
  if ! grep -q '^SIMILAR=' "$ENVF"; then
    echo "==> Ähnlichkeitsanalyse in $ENVF ergänzt (SIMILAR=…; leer setzen zum Abschalten)"
    printf '%s\n' '# Stufe 4 – ähnliche Inhalte (Bilder, Videos, Audio, Texte, PDFs, Office, Archive). Leer lassen = aus.' \
      'SIMILAR=-similar -analyze-workers 6 -media-workers 2 -sim-max 512' >> "$ENVF"
  elif grep -qx 'SIMILAR=-similar -analyze-workers 4 -sim-max 512' "$ENVF"; then
    echo "==> Ähnlichkeitsanalyse: Standardwerte aktualisiert (mehr Worker, eigener Video/Audio-Pool)"
    sed -i 's|^SIMILAR=-similar -analyze-workers 4 -sim-max 512$|SIMILAR=-similar -analyze-workers 6 -media-workers 2 -sim-max 512|' "$ENVF"
  fi
  if ! grep -q '^# -max-rate' "$ENVF"; then
    printf '%s\n' '# -max-rate 50 in EXTRA begrenzt die gesamte Download-Rate auf 50 MB/s (z.B. auf geteilten Servern)' >> "$ENVF"
  fi
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
ExecStart=$APP/web-dupplet -db $DATA/dupplet.db -listen \${LISTEN} -auth \${AUTH} \$WORKERS \$SIMILAR \$EXTRA
Restart=on-failure
RestartSec=5
KillSignal=SIGINT
TimeoutStopSec=30
LimitNOFILE=1048576
# Ressourcen: max. ~85 % der CPU-Kerne
CPUQuota=$(( $(nproc) * 85 ))%
# Niedrigere Priorität: andere Dienste auf dem Server haben bei CPU und Festplatte Vorrang
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
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

LISTEN_ADDR=$(grep '^LISTEN=' "$ENVF" | cut -d= -f2)
case "$LISTEN_ADDR" in
  127.*|localhost:*|\[::1\]:*)
    echo "==> Firewall: nichts freigegeben (Web-UI lauscht nur lokal auf $LISTEN_ADDR)" ;;
  *)
    if command -v ufw >/dev/null && ufw status | grep -q "Status: active"; then
      echo "==> Firewall (ufw): Port $PORT freigeben"; ufw allow "$PORT"/tcp >/dev/null
    elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
      echo "==> Firewall (firewalld): Port $PORT freigeben"; firewall-cmd -q --permanent --add-port="$PORT"/tcp; firewall-cmd -q --reload
    fi ;;
esac

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
case "$LISTEN_ADDR" in
  127.*|localhost:*) echo "   Web-UI:    http://$LISTEN_ADDR  (nur lokal – z.B. per SSH-Tunnel: ssh -L $LISTEN_PORT:$LISTEN_ADDR <server>)" ;;
  *)                 echo "   Web-UI:    http://$IP:$LISTEN_PORT" ;;
esac
echo "   Benutzer:  ${CRED%%:*}"
echo "   Passwort:  ${CRED#*:}"
echo
echo "   Test-Quellen starten:  $APP/quellen-demo.sh"
echo "   Log ansehen:           journalctl -u web-dupplet -f"
echo "   Stoppen/Starten:       sudo systemctl stop|start web-dupplet"
echo "   Einstellungen:         sudo nano $ENVF"
