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

## Stufe 4: Ähnliche Inhalte (`-similar`)

Findet Dateien, die **inhaltlich gleich oder fast gleich** sind, auch wenn kein einziges Byte übereinstimmt.
Ergebnisse stehen in der Web-UI unter „Ähnliche Inhalte“ (mit Vorschau nebeneinander und Prozentwert).

| Art | Erkennt | Verfahren |
|---|---|---|
| Bilder (JPG, PNG, GIF, BMP, WebP, TIFF) | andere Auflösung, anderes Format, stärkere Kompression | pHash (DCT, 64 Bit) + dHash als Bestätigung |
| Videos (MP4, MKV, WebM, AVI, MOV, …) | neu kodiert, andere Qualität/Auflösung/Container | pHash von 8 Einzelbildern (ffmpeg) + Länge ±2 % |
| Texte, Office, PDF | gleicher oder fast gleicher Text in TXT/HTML/RTF/DOCX/ODT/PPTX/XLSX/PDF | SimHash über Wort-Dreiergruppen |
| Bilder in PDFs und Office-Dateien | eingebettete Bilder, die es auch einzeln gibt | wie Bilder (pdfimages bzw. ZIP-Inhalt) |
| Archive (ZIP, JAR, EPUB, TAR, TAR.GZ, TAR.BZ2) | Dateien, die verpackt und unverpackt vorkommen | SHA-256 je Eintrag + Inhaltsanalyse |

Schwellen: Bilder ≤ 8 von 64 Bit (pHash) und ≤ 14 (dHash), Texte ≤ 7 Bit, Videos ≥ 75 % der Einzelbilder ≤ 10 Bit.
Zum Vergleich: zufällig verschiedene Inhalte liegen bei etwa 32 Bit.

- **Kosten:** Bilder, Dokumente und Archive werden komplett geladen (bis `-sim-max`, Standard 512 MB).
  Videos nicht: ffmpeg springt per Range-Anfrage zu den Einzelbildern. Kann der Server keine Ranges,
  wird das Video komplett geladen (ebenfalls bis `-sim-max`).
- **Benötigt:** `ffmpeg` (Videos) und `poppler-utils` (PDFs). Fehlen sie, werden diese Typen übersprungen
  und die Web-UI weist darauf hin. `install.sh` installiert beides.
- **Nicht doppelt:** Byte-gleiche Dateien stehen in den Duplikat-Gruppen bzw. als „exakt, verpackt“ –
  sie erscheinen nicht zusätzlich als „ähnlich“.
- Eigene DB-Verbindung und eigene Worker (`-analyze-workers`), die Stufen 1–3 werden nicht ausgebremst.

API: `GET /api/similar?kind=0..4&offset=&limit=` (1 Bild, 2 Text, 3 Video, 4 Archiv), `GET /api/thumb?fp=`,
`GET /api/similar.csv`, Zahlen unter `sim` in `/api/stats`.

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
| `-similar` | aus | Stufe 4: ähnliche Inhalte finden (siehe oben) |
| `-analyze-workers` | 4 | Parallele Ähnlichkeitsanalysen (CPU-lastig) |
| `-sim-max` | 512 | Größere Dateien (MB) nicht für die Ähnlichkeit laden |
| `-video-frames` | 8 | Einzelbilder pro Video |

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
./build.sh          # nur für diesen Rechner
./paket.sh          # Installationspaket dist/web-dupplet.tar.gz (x86_64 + ARM64)
go test ./...       # Tests (Video-Test braucht ffmpeg)
```
Go liegt unter `~/.local/go` (ohne Root installiert).
