package main

// Inhalte für die Ähnlichkeitsanalyse herausziehen: Dateityp erkennen, Bilder dekodieren,
// Text aus Office/PDF/HTML/RTF, Bilder aus PDFs, Einzelbilder aus Videos, Archive entpacken.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// Medientypen
const (
	mtNone = iota
	mtImage
	mtVideo
	mtText
	mtOffice
	mtPDF
	mtArchive
)

var mtName = map[int]string{mtImage: "Bild", mtVideo: "Video", mtText: "Text", mtOffice: "Office", mtPDF: "PDF", mtArchive: "Archiv"}

var extType = func() map[string]int {
	m := map[string]int{}
	set := func(t int, exts ...string) {
		for _, e := range exts {
			m[e] = t
		}
	}
	set(mtImage, "jpg", "jpeg", "jpe", "jfif", "png", "gif", "bmp", "webp", "tif", "tiff")
	set(mtVideo, "mp4", "m4v", "mkv", "webm", "avi", "mov", "wmv", "flv", "mpg", "mpeg", "ts", "m2ts", "3gp", "ogv", "vob")
	set(mtText, "txt", "text", "md", "markdown", "csv", "tsv", "log", "json", "xml", "html", "htm", "xhtml", "rtf", "tex", "rst", "asc", "nfo")
	set(mtOffice, "docx", "docm", "odt", "pptx", "odp", "xlsx", "ods")
	set(mtPDF, "pdf")
	set(mtArchive, "zip", "jar", "epub", "tar", "tgz", "tbz", "tbz2", "tar.gz", "tar.bz2")
	return m
}()

// extOf liefert die Endung in Kleinbuchstaben ("tar.gz" als Ganzes), bei URLs ohne Query.
func extOf(loc string) string {
	p := loc
	if u, err := url.Parse(loc); err == nil && u.Scheme != "" && u.Path != "" {
		p = u.Path
	}
	b := strings.ToLower(path.Base(filepath.ToSlash(p)))
	for _, two := range []string{".tar.gz", ".tar.bz2"} {
		if strings.HasSuffix(b, two) {
			return two[1:]
		}
	}
	if i := strings.LastIndexByte(b, '.'); i >= 0 && i < len(b)-1 {
		return b[i+1:]
	}
	return ""
}

func classify(loc string) int { return extType[extOf(loc)] }

// ---------- Werkzeuge ----------

type tools struct{ ffmpeg, ffprobe, pdftotext, pdfimages string }

func findTools() tools {
	look := func(n string) string { p, _ := exec.LookPath(n); return p }
	return tools{look("ffmpeg"), look("ffprobe"), look("pdftotext"), look("pdfimages")}
}

func runTool(ctx context.Context, d time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if ctx.Err() != nil {
			msg = "Zeitlimit überschritten"
		}
		return out, fmt.Errorf("%s: %v %s", filepath.Base(name), err, msg)
	}
	return out, nil
}

// ---------- Bilder ----------

const maxPixels = 80_000_000 // Schutz vor „Dekompressionsbomben“

func decodeImage(b []byte) (image.Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, fmt.Errorf("Bild zu groß (%d×%d)", cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(b))
	return img, err
}

// ---------- Text ----------

var (
	reTags = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>|<[^>]*>`)
)

func htmlText(s string) string { return html.UnescapeString(reTags.ReplaceAllString(s, " ")) }

// rtfSkip: Gruppen ohne sichtbaren Text (Schrift-/Farbtabellen, Metadaten, Bilder …)
var rtfSkip = map[string]bool{"fonttbl": true, "colortbl": true, "stylesheet": true, "info": true, "pict": true,
	"header": true, "footer": true, "listtable": true, "listoverridetable": true, "rsidtbl": true, "generator": true,
	"themedata": true, "colorschememapping": true, "latentstyles": true, "datastore": true, "xmlnstbl": true}

// rtfText: kleiner RTF-Leser – überspringt unsichtbare Gruppen, dekodiert \'xx (Windows-1252 ≈ Latin-1)
// und \uN (Unicode).
func rtfText(s string) string {
	var sb strings.Builder
	type grp struct{ skip bool }
	stack := []grp{{}}
	skipping := func() bool { return stack[len(stack)-1].skip }
	newGroup := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '{':
			stack = append(stack, grp{skip: skipping()})
			newGroup = true
			continue
		case '}':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			newGroup = false
			continue
		case '\r', '\n':
			continue
		case '\\':
			if i+1 >= len(s) {
				continue
			}
			n := s[i+1]
			switch {
			case n == '*':
				stack[len(stack)-1].skip = true
				i++
			case n == '\'' && i+3 < len(s):
				if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil && !skipping() {
					sb.WriteRune(rune(v))
				}
				i += 3
			case (n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z'):
				j := i + 1
				for j < len(s) && ((s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z')) {
					j++
				}
				word := s[i+1 : j]
				k := j
				if k < len(s) && (s[k] == '-' || (s[k] >= '0' && s[k] <= '9')) {
					k++
					for k < len(s) && s[k] >= '0' && s[k] <= '9' {
						k++
					}
				}
				param := s[j:k]
				if k < len(s) && s[k] == ' ' {
					k++ // Leerzeichen gehört zum Steuerwort
				}
				i = k - 1
				if newGroup && rtfSkip[word] {
					stack[len(stack)-1].skip = true
				}
				if !skipping() {
					switch word {
					case "par", "line", "tab", "cell", "row", "sect", "page":
						sb.WriteByte(' ')
					case "u":
						if v, err := strconv.Atoi(param); err == nil {
							if v < 0 {
								v += 65536
							}
							sb.WriteRune(rune(v))
							if i+1 < len(s) && s[i+1] != '\\' && s[i+1] != '{' && s[i+1] != '}' {
								i++ // Ersatzzeichen nach \uN überspringen
							}
						}
					}
				}
			default: // \\ \{ \} und andere Symbole
				if !skipping() && (n == '\\' || n == '{' || n == '}') {
					sb.WriteByte(n)
				}
				i++
			}
			newGroup = false
			continue
		}
		newGroup = false
		if !skipping() {
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// plainText: Textdateien je nach Endung aufbereiten.
func plainText(ext string, b []byte) string {
	s := toUTF8(b)
	switch ext {
	case "html", "htm", "xhtml", "xml":
		return htmlText(s)
	case "rtf":
		return rtfText(s)
	}
	return s
}

// xmlText sammelt allen Zeichentext eines XML-Dokuments.
func xmlText(r io.Reader, limit int) string {
	var sb strings.Builder
	d := xml.NewDecoder(r)
	d.Strict = false
	for sb.Len() < limit {
		t, err := d.Token()
		if err != nil {
			break
		}
		switch v := t.(type) {
		case xml.CharData:
			sb.Write(v)
		case xml.EndElement: // Absätze/Zellen trennen
			switch v.Name.Local {
			case "p", "t", "tc", "h", "si", "c", "span":
				sb.WriteByte(' ')
			}
		}
	}
	return sb.String()
}

const maxText = 8 << 20

// officeText: DOCX/PPTX/XLSX (Office Open XML) und ODT/ODP/ODS (OpenDocument) sind ZIP-Archive mit XML.
func officeText(ra io.ReaderAt, size int64) (string, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return "", err
	}
	var parts []*zip.File
	for _, f := range zr.File {
		n := f.Name
		if n == "word/document.xml" || n == "content.xml" || n == "xl/sharedStrings.xml" ||
			(strings.HasPrefix(n, "ppt/slides/slide") && strings.HasSuffix(n, ".xml")) {
			parts = append(parts, f)
		}
	}
	if len(parts) == 0 {
		return "", errors.New("kein Textinhalt im Office-Dokument")
	}
	sort.Slice(parts, func(i, j int) bool { return natLess(parts[i].Name, parts[j].Name) })
	var sb strings.Builder
	for _, f := range parts {
		rc, err := f.Open()
		if err != nil {
			continue
		}
		sb.WriteString(xmlText(io.LimitReader(rc, 64<<20), maxText-sb.Len()))
		sb.WriteByte('\n')
		rc.Close()
	}
	return sb.String(), nil
}

// natLess sortiert slide2 vor slide10.
func natLess(a, b string) bool {
	na, nb := trailingNum(a), trailingNum(b)
	if na >= 0 && nb >= 0 && strings.TrimRight(strings.TrimSuffix(a, ".xml"), "0123456789") == strings.TrimRight(strings.TrimSuffix(b, ".xml"), "0123456789") {
		return na < nb
	}
	return a < b
}

func trailingNum(s string) int {
	s = strings.TrimSuffix(s, ".xml")
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return -1
	}
	return n
}

// ---------- PDF ----------

func (t tools) pdfText(ctx context.Context, file string) (string, error) {
	if t.pdftotext == "" {
		return "", errors.New("pdftotext fehlt (Paket poppler-utils)")
	}
	out, err := runTool(ctx, 3*time.Minute, t.pdftotext, "-q", "-enc", "UTF-8", "-l", "2000", file, "-")
	if len(out) > maxText {
		out = out[:maxText]
	}
	return dehyphen(string(out)), err
}

var reHyphen = regexp.MustCompile(`(\p{L})[-\x{00AD}]\r?\n[ \t]*(\p{Ll})`)

// dehyphen fügt am Zeilenende getrennte Wörter wieder zusammen („Fingerab-⏎drücke“ → „Fingerabdrücke“).
func dehyphen(s string) string { return reHyphen.ReplaceAllString(s, "$1$2") }

// pdfImages zieht eingebettete Bilder heraus (JPEG bleibt JPEG, alles andere wird PNG).
func (t tools) pdfImages(ctx context.Context, file, dir string, maxImgs int) ([]string, error) {
	if t.pdfimages == "" {
		return nil, errors.New("pdfimages fehlt (Paket poppler-utils)")
	}
	// -l begrenzt die Seiten: bei eingescannten PDFs ist jede Seite ein Bild in voller Auflösung
	if _, err := runTool(ctx, 3*time.Minute, t.pdfimages, "-q", "-j", "-png", "-l", strconv.Itoa(maxImgs), file, filepath.Join(dir, "img")); err != nil {
		return nil, err
	}
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "img") && len(out) < maxImgs {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---------- Video ----------

// VideoFP: pHash je Einzelbild (0 = Bild unbrauchbar, z.B. schwarz) + Dauer + Vorschaubild.
type VideoFP struct {
	Frames []uint64
	Dur    float64
	Thumb  []byte
}

func (t tools) videoFingerprint(ctx context.Context, src, ua string, n int) (fp VideoFP, err error) {
	if t.ffmpeg == "" || t.ffprobe == "" {
		return fp, errors.New("ffmpeg/ffprobe fehlt")
	}
	isURL := strings.Contains(src, "://")
	in := func(args ...string) []string {
		a := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
		if isURL {
			a = append(a, "-user_agent", ua, "-rw_timeout", "30000000")
		}
		return append(a, args...)
	}
	probeArgs := []string{"-v", "error", "-show_entries", "format=duration", "-of", "default=nw=1:nk=1"}
	if isURL {
		probeArgs = append(probeArgs, "-user_agent", ua, "-rw_timeout", "30000000")
	}
	out, err := runTool(ctx, 90*time.Second, t.ffprobe, append(probeArgs, src)...)
	if err != nil {
		return fp, err
	}
	fp.Dur, err = strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || fp.Dur <= 0 {
		return fp, errors.New("Videolänge unbekannt")
	}
	valid := 0
	for i := 0; i < n; i++ {
		ts := fp.Dur * (float64(i) + 0.5) / float64(n)
		raw, err := runTool(ctx, 90*time.Second, t.ffmpeg, in("-ss", strconv.FormatFloat(ts, 'f', 3, 64), "-i", src,
			"-frames:v", "1", "-vf", "scale=32:32:flags=area,format=gray", "-f", "rawvideo", "-")...)
		var h uint64
		if err == nil && len(raw) == hashSide*hashSide {
			px := make([]float64, len(raw))
			for k, v := range raw {
				px[k] = float64(v)
			}
			var ok bool
			if h, ok = pHashGray(px); ok {
				valid++
			}
		} else if err != nil && ctx.Err() != nil {
			return fp, err
		}
		fp.Frames = append(fp.Frames, h)
	}
	if valid < 2 {
		return fp, errors.New("zu wenige auswertbare Einzelbilder")
	}
	// Vorschaubild aus der Mitte
	if jpg, err := runTool(ctx, 90*time.Second, t.ffmpeg, in("-ss", strconv.FormatFloat(fp.Dur/2, 'f', 3, 64), "-i", src,
		"-frames:v", "1", "-vf", "scale=160:-2", "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "6", "-")...); err == nil {
		fp.Thumb = jpg
	}
	return fp, nil
}

func framesBlob(f []uint64) []byte {
	b := make([]byte, 8*len(f))
	for i, h := range f {
		binary.LittleEndian.PutUint64(b[8*i:], h)
	}
	return b
}

func blobFrames(b []byte) []uint64 {
	f := make([]uint64, len(b)/8)
	for i := range f {
		f[i] = binary.LittleEndian.Uint64(b[8*i:])
	}
	return f
}

// videoScore vergleicht zwei Videos Bild für Bild. ok, wenn ≥ 75 % der gemeinsam
// auswertbaren Einzelbilder ähnlich sind und die Länge um höchstens 2 % (min. 1 s) abweicht.
func videoScore(a, b []uint64, da, db float64) (score float64, ok bool) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, false
	}
	if diff := abs(da - db); diff > max(1, 0.02*max(da, db)) {
		return 0, false
	}
	var n, good, sum int
	for i := range a {
		if a[i] == 0 || b[i] == 0 {
			continue
		}
		n++
		d := hamming(a[i], b[i])
		sum += d
		if d <= vidFrameD {
			good++
		}
	}
	if n < 2 || good*4 < n*3 {
		return 0, false
	}
	return 1 - float64(sum)/float64(n)/64, true
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// ---------- Archive ----------

const (
	maxMembers    = 20000
	maxMemberSize = 1 << 30 // größere Einträge werden übersprungen
)

// walkArchive ruft fn für jede Datei im Archiv auf (ZIP-Familie, TAR, TAR.GZ, TAR.BZ2).
func walkArchive(file, ext string, fn func(name string, size int64, r io.Reader) error) error {
	switch ext {
	case "zip", "jar", "epub":
		zr, err := zip.OpenReader(file)
		if err != nil {
			return err
		}
		defer zr.Close()
		n := 0
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || f.UncompressedSize64 > maxMemberSize {
				continue
			}
			if n++; n > maxMembers {
				break
			}
			rc, err := f.Open()
			if err != nil {
				continue // z.B. verschlüsselt oder unbekanntes Verfahren
			}
			err = fn(f.Name, int64(f.UncompressedSize64), io.LimitReader(rc, maxMemberSize))
			rc.Close()
			if err != nil {
				return err
			}
		}
		return nil
	}
	fd, err := os.Open(file)
	if err != nil {
		return err
	}
	defer fd.Close()
	var r io.Reader = fd
	switch ext {
	case "tgz", "tar.gz":
		gz, err := gzip.NewReader(fd)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	case "tbz", "tbz2", "tar.bz2":
		r = bzip2.NewReader(fd)
	}
	tr := tar.NewReader(r)
	for n := 0; n < maxMembers; {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || h.Size > maxMemberSize {
			continue
		}
		n++
		if err := fn(h.Name, h.Size, tr); err != nil {
			return err
		}
	}
	return nil
}
