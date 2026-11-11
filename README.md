# Web-Dupplet

Findet **identische Dateien** im Netz (HTTP/HTTPS) und optional in lokalen Ordnern –
ausgelegt auf Datenmengen von **100 TB und mehr**. Mit Web-Oberfläche, die live zeigt,
wie viele Dateien schon angeschaut wurden und wie viele Übereinstimmungen es gibt.

## Schnellstart

```bash
cd ~/Web-Dupplet
./web-dupplet -urls meine-urls.txt          # eine URL pro Zeile
# Browser: http://127.0.0.1:8080
```

Weitere URLs kann man jederzeit in der Web-UI einfügen. Strg+C beendet sauber; beim nächsten
Start geht es genau dort weiter (alles steht in `dupplet.db`).

```bash
# Zugriff aus dem LAN, mit Passwort:
./web-dupplet -listen 0.0.0.0:8080 -auth admin:geheim -urls urls.txt

# Netz-Dateien zusätzlich mit lokalem Bestand vergleichen:
./web-dupplet -urls urls.txt -scan /mnt/archiv

# Dateien wirklich herunterladen und behalten (Duplikate belegen nur 1x Platz):
./web-dupplet -urls urls.txt -store /mnt/downloads
```

## Warum es schnell ist – der gestaffelte Vergleich

Bei 100 TB ist nicht die CPU, sondern **die Bandbreite** der Engpass. Deshalb wird nur so viel
geladen, wie zur sicheren Entscheidung nötig ist:

| Stufe | Was passiert | Traffic pro Datei |
|---|---|---|
| 1. Größe | `HEAD`-Request. Einmalige Größe ⇒ **kann kein Duplikat sein**, fertig. | ~0 Byte |
| 2. Teil-Hash | Nur bei gleicher Größe: erste + letzte 64 KiB per Range-Request | 128 KiB |
| 3. Voll-Hash | Nur bei gleichem Teil-Hash: komplett laden, SHA-256 | ganze Datei |

**Übereinstimmung = identischer SHA-256 über den gesamten Inhalt.** Stufe 1 und 2 sind nur
Abkürzungen, um eindeutige Dateien auszusortieren – sie erzeugen nie eine falsche Übereinstimmung.

Wichtig: „eindeutig nach Größe“ ist vorläufig. Kommt später eine Datei mit gleicher Größe dazu,
wird die ältere automatisch nachgeprüft.

Weitere Details:
- **Ein einziger Datenbank-Schreiber** mit Sammel-Transaktionen (bis 1024 Ergebnisse pro Commit),
  SQLite im WAL-Modus. Gemessen: ~6.500 Dateien/s bei 200.000 lokalen Dateien.
- Alle Prüfungen auf Duplikatgruppen sind O(1) pro Datei – auch wenn Millionen Dateien gleich groß sind.
- SHA-256 nutzt die Hardware-Beschleunigung der CPU (SHA-NI, auf diesem Server vorhanden).
- Downloads werden gestreamt und nicht zwischengespeichert (außer mit `-store`).
- Keep-Alive / HTTP/2, Begrenzung pro Server (`-per-host`), Abbruch bei hängenden Downloads,
  automatische Wiederholung mit Wartezeit bei vorübergehenden Fehlern.
- Server ohne Range-Unterstützung werden erkannt (dann wird komplett geladen).
- Nach einem Absturz/Neustart wird nichts doppelt geladen.

## Optionen

| Option | Standard | Bedeutung |
|---|---|---|
| `-urls DATEI` | – | URL-Liste (eine pro Zeile, `#` = Kommentar, `-` = stdin). Mehrfach möglich. |
| `-scan ORDNER` | – | Lokalen Ordner rekursiv mit vergleichen. Mehrfach möglich. |
| `-db DATEI` | `dupplet.db` | Datenbank mit dem gesamten Fortschritt |
| `-listen ADR` | `127.0.0.1:8080` | Adresse der Web-UI |
| `-auth user:pw` | – | Passwortschutz der Web-UI (dringend empfohlen bei `0.0.0.0`) |
| `-store ORDNER` | – | Downloads behalten, abgelegt als `objects/ab/cd/<sha256>` (Duplikate nur 1x) |
| `-full` | aus | Jede Datei komplett hashen (keine Abkürzungen) |
| `-probe-workers` | 64 | Parallele Größen-Abfragen |
| `-fetch-workers` | 128 | Parallele Downloads |
| `-local-workers` | 4 | Parallele lokale Leser (HDD: 1–2, NVMe: 8–16) |
| `-per-host` | 16 | Max. Download-Verbindungen pro Server (Höflichkeit/Rate-Limits) |
| `-probe-per-host` | 16 | Max. gleichzeitige Größenabfragen (HEAD) pro Server |
| `-crawl URL` | – | Webseite oder Dateiliste (z.B. `IndexByDate.txt`) nach Dateien durchsuchen |
| `-ext` | * | Dateitypen für `-crawl`: `*` = alle, oder `word`, `bilder`, `videos`, `audio`, `office`, `archive`, bzw. Endungen wie `pdf,docx,jpg` |
| `-timeout` | 60s | Abbruch, wenn so lange keine Daten kommen |
| `-retries` | 3 | Versuche bei vorübergehenden Fehlern |

Größenordnung für 100 TB: Die Datenbank braucht ca. 150–250 Byte pro Datei
(10 Mio. Dateien ≈ 2–3 GB). Der eigentliche Engpass ist die Netzwerkanbindung.

## Web-UI / API

- `GET /` – Dashboard (aktualisiert sich jede Sekunde)
- `GET /api/stats` – alle Zahlen als JSON
- `GET /api/groups?offset=0&limit=50` – Duplikatgruppen, sortiert nach verschwendetem Platz
- `GET /api/export.csv` – alle Duplikate als CSV (sha256, anzahl, größe, ort)
- `POST /api/add` – URLs als Text (eine pro Zeile), z.B. `curl --data-binary @urls.txt localhost:8080/api/add`
- `POST /api/scan` – `{"path":"/mnt/x"}`
- `POST /api/pause?on=1` / `?on=0`

## Neu bauen

```bash
./build.sh
```
Go liegt unter `~/.local/go` (ohne Root installiert).
