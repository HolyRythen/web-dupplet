package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]int{
		"https://x.de/a/Foto.JPG":            mtImage,
		"https://x.de/v.mp4?token=1":         mtVideo,
		"/daten/bericht.docx":                mtOffice,
		"https://x.de/doku.pdf#seite=3":      mtPDF,
		"https://x.de/pkg/hello-2.12.tar.gz": mtArchive,
		"https://x.de/index.html":            mtText,
		"https://x.de/programm.exe":          mtNone,
		"https://x.de/ordner/":               mtNone,
	}
	for loc, want := range cases {
		if got := classify(loc); got != want {
			t.Errorf("%s: %d, erwartet %d", loc, got, want)
		}
	}
}

// zipMit baut ein ZIP im Speicher.
func zipMit(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(c))
	}
	zw.Close()
	return buf.Bytes()
}

func TestOfficeText(t *testing.T) {
	docx := zipMit(t, map[string]string{
		"[Content_Types].xml": "<Types/>",
		"word/document.xml": `<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>Hallo</w:t></w:r><w:r><w:t>Welt</w:t></w:r></w:p>` +
			`<w:p><w:r><w:t>Zweiter Absatz &amp; mehr</w:t></w:r></w:p></w:body></w:document>`,
	})
	s, err := officeText(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(normWords(s), " "); w != "hallo welt zweiter absatz mehr" {
		t.Fatalf("DOCX-Text: %q", w)
	}
	odt := zipMit(t, map[string]string{"content.xml": `<office:document-content><text:p>Offenes Dokument</text:p></office:document-content>`})
	s, _ = officeText(bytes.NewReader(odt), int64(len(odt)))
	if !strings.Contains(s, "Offenes Dokument") {
		t.Fatalf("ODT-Text: %q", s)
	}
}

func TestDocxUndTxtMitGleichemTextSindAehnlich(t *testing.T) {
	var paras strings.Builder
	for _, satz := range strings.Split(beispielText, "\n") {
		paras.WriteString("<w:p><w:r><w:t>" + satz + "</w:t></w:r></w:p>")
	}
	docx := zipMit(t, map[string]string{"word/document.xml": "<w:document><w:body>" + paras.String() + "</w:body></w:document>"})
	s, err := officeText(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := textFingerprint(s)
	b, _ := textFingerprint(beispielText)
	if d := hamming(a.Sim, b.Sim); d != 0 {
		t.Fatalf("DOCX und TXT mit gleichem Text: Abstand %d, erwartet 0", d)
	}
	h, _ := textFingerprint(htmlText("<html><head><style>p{color:red}</style></head><body><p>" +
		strings.ReplaceAll(beispielText, "\n", "</p><p>") + "</p><script>var x=1;</script></body></html>"))
	if d := hamming(h.Sim, b.Sim); d != 0 {
		t.Fatalf("HTML und TXT mit gleichem Text: Abstand %d, erwartet 0", d)
	}
}

func TestRtfText(t *testing.T) {
	s := rtfText(`{\rtf1\ansi{\fonttbl{\f0 Arial;}}{\*\generator Word;}\f0\fs24 Hallo \b Welt\b0  und Gr\'fc\'dfe\par}`)
	w := strings.Join(normWords(s), " ")
	if !strings.Contains(w, "hallo welt und") || strings.Contains(w, "arial") || strings.Contains(w, "generator") {
		t.Fatalf("RTF-Text: %q", w)
	}
}

func TestWalkArchive(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "a.zip")
	os.WriteFile(zp, zipMit(t, map[string]string{"ordner/eins.txt": "eins", "zwei.bin": "zwei!"}), 0o644)

	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	for n, c := range map[string]string{"x/drei.txt": "drei"} {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	tw.Close()
	gz.Close()
	tp := filepath.Join(dir, "b.tar.gz")
	os.WriteFile(tp, tgz.Bytes(), 0o644)

	for file, want := range map[string]map[string]string{zp: {"ordner/eins.txt": "eins", "zwei.bin": "zwei!"}, tp: {"x/drei.txt": "drei"}} {
		got := map[string]string{}
		err := walkArchive(file, extOf(file), func(n string, size int64, r io.Reader) error {
			var b bytes.Buffer
			b.ReadFrom(r)
			if int64(b.Len()) != size {
				t.Errorf("%s: Größe %d ≠ %d", n, b.Len(), size)
			}
			got[n] = b.String()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for n, c := range want {
			if got[n] != c {
				t.Errorf("%s: %s = %q, erwartet %q", filepath.Base(file), n, got[n], c)
			}
		}
	}
}

func TestVideoScore(t *testing.T) {
	a := []uint64{0xF0F0, 0xAAAA, 0, 0x1234, 0xFFFF0000, 0x0F0F0F0F, 0x55, 0x99}
	b := append([]uint64{}, a...)
	b[1] ^= 0b111 // 3 Bits Unterschied
	if s, ok := videoScore(a, b, 120, 120.5); !ok || s < 0.98 {
		t.Fatalf("fast gleiches Video: ok=%v score=%.3f", ok, s)
	}
	if _, ok := videoScore(a, b, 120, 180); ok {
		t.Fatal("unterschiedliche Länge darf nicht passen")
	}
	c := make([]uint64, len(a))
	for i := range a {
		c[i] = ^a[i]
	}
	if _, ok := videoScore(a, c, 120, 120); ok {
		t.Fatal("völlig anderes Video darf nicht passen")
	}
}

// Echter Test mit ffmpeg: dasselbe Video in zwei Qualitäten/Formaten ist ähnlich, ein anderes nicht.
func TestVideoFingerprintMitFFmpeg(t *testing.T) {
	tl := findTools()
	if tl.ffmpeg == "" || tl.ffprobe == "" {
		t.Skip("ffmpeg nicht installiert")
	}
	dir := t.TempDir()
	mk := func(name string, args ...string) string {
		p := filepath.Join(dir, name)
		cmd := exec.Command(tl.ffmpeg, append(append([]string{"-nostdin", "-loglevel", "error", "-y"}, args...), p)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %s: %v %s", name, err, out)
		}
		return p
	}
	src := "testsrc2=size=640x360:rate=25:duration=12"
	hq := mk("hq.mp4", "-f", "lavfi", "-i", src, "-c:v", "libx264", "-crf", "20", "-pix_fmt", "yuv420p")
	lq := mk("lq.webm", "-f", "lavfi", "-i", src, "-vf", "scale=320:180", "-c:v", "libvpx", "-b:v", "150k")
	other := mk("anders.mp4", "-f", "lavfi", "-i", "mandelbrot=size=640x360:rate=25", "-t", "12", "-c:v", "libx264", "-pix_fmt", "yuv420p")

	ctx := context.Background()
	fa, err := tl.videoFingerprint(ctx, hq, "test", 8)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := tl.videoFingerprint(ctx, lq, "test", 8)
	if err != nil {
		t.Fatal(err)
	}
	fo, err := tl.videoFingerprint(ctx, other, "test", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(fa.Thumb) == 0 {
		t.Error("kein Vorschaubild")
	}
	if s, ok := videoScore(fa.Frames, fb.Frames, fa.Dur, fb.Dur); !ok {
		t.Errorf("HQ-MP4 und LQ-WebM sollten ähnlich sein (Score %.2f)", s)
	}
	if s, ok := videoScore(fa.Frames, fo.Frames, fa.Dur, fo.Dur); ok {
		t.Errorf("anderes Video gilt fälschlich als ähnlich (Score %.2f)", s)
	}
}

func TestDehyphen(t *testing.T) {
	got := dehyphen("Die Fingerab-\ndrücke bleiben gleich. Nord-\nSüd bleibt getrennt.")
	if got != "Die Fingerabdrücke bleiben gleich. Nord-\nSüd bleibt getrennt." {
		t.Fatalf("%q", got)
	}
}

// Echter Test mit ffmpeg: dasselbe Musikstück als MP3, OGG und Opus in verschiedenen Bitraten ist ähnlich,
// ein anderes Stück nicht.
func TestAudioFingerprintMitFFmpeg(t *testing.T) {
	tl := findTools()
	if tl.ffmpeg == "" || tl.ffprobe == "" {
		t.Skip("ffmpeg nicht installiert")
	}
	dir := t.TempDir()
	// „Musik“: Akkordfolge mit wechselnden Tönen + leises Rauschen, 60 s
	stueck := "aevalsrc=exprs='0.3*sin(2*PI*(220+55*mod(floor(t*2),7))*t)+0.2*sin(2*PI*(330+40*mod(floor(t*3),5))*t)+0.15*sin(2*PI*(523+30*mod(floor(t),4))*t)+0.02*(random(0)-0.5)':s=44100:d=60"
	anderes := "aevalsrc=exprs='0.3*sin(2*PI*(392+70*mod(floor(t*1.5),6))*t)+0.25*sin(2*PI*(147+20*mod(floor(t*4),3))*t)+0.02*(random(0)-0.5)':s=44100:d=60"
	mk := func(name, src string, args ...string) string {
		p := filepath.Join(dir, name)
		a := append([]string{"-nostdin", "-loglevel", "error", "-y", "-f", "lavfi", "-i", src}, args...)
		if out, err := exec.Command(tl.ffmpeg, append(a, p)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %s: %v %s", name, err, out)
		}
		return p
	}
	mp3 := mk("a.mp3", stueck, "-c:a", "libmp3lame", "-b:a", "192k")
	ogg := mk("a.ogg", stueck, "-c:a", "libvorbis", "-q:a", "1")
	opus := mk("a.opus", stueck, "-c:a", "libopus", "-b:a", "32k")
	fremd := mk("b.mp3", anderes, "-c:a", "libmp3lame", "-b:a", "128k")
	ctx := context.Background()
	fp := func(p string) AudioFP {
		f, err := tl.audioFingerprint(ctx, p, "test", 8)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return f
	}
	a, b, c, x := fp(mp3), fp(ogg), fp(opus), fp(fremd)
	for name, o := range map[string]AudioFP{"OGG": b, "Opus": c, "fremd": x} {
		var d []int
		for i := range a.Segs {
			d = append(d, hamming(a.Segs[i], o.Segs[i]))
		}
		t.Logf("MP3 ↔ %-5s Bit-Abstände je Ausschnitt: %v", name, d)
	}
	for name, o := range map[string]AudioFP{"OGG q1": b, "Opus 32k": c} {
		if s, ok := seqScore(a.Segs, o.Segs, a.Dur, o.Dur, audFrameD); !ok {
			t.Errorf("MP3 192k und %s sollten ähnlich sein (Score %.2f)", name, s)
		}
	}
	if s, ok := seqScore(a.Segs, x.Segs, a.Dur, x.Dur, audFrameD); ok {
		t.Errorf("anderes Stück gilt fälschlich als ähnlich (Score %.2f)", s)
	}
}
