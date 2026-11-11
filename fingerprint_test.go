package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

// testBild erzeugt ein „natürlich“ strukturiertes Bild (Verlauf + zufällige Formen).
func testBild(seed int64, w, h int) *image.RGBA {
	rnd := rand.New(rand.NewSource(seed))
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), 120, 255})
		}
	}
	for k := 0; k < 25; k++ {
		cx, cy, r := rnd.Intn(w), rnd.Intn(h), 10+rnd.Intn(w/5)
		c := color.RGBA{uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), 255}
		for y := max(0, cy-r); y < min(h, cy+r); y++ {
			for x := max(0, cx-r); x < min(w, cx+r); x++ {
				if (x-cx)*(x-cx)+(y-cy)*(y-cy) < r*r {
					img.Set(x, y, c)
				}
			}
		}
	}
	return img
}

func verkleinere(img image.Image, w, h int) image.Image {
	r, g, b := boxResize(img, w, h)
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range r {
		out.Pix[4*i], out.Pix[4*i+1], out.Pix[4*i+2], out.Pix[4*i+3] = uint8(r[i]), uint8(g[i]), uint8(b[i]), 255
	}
	return out
}

func alsJPEG(t *testing.T, img image.Image, q int) image.Image {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	out, err := jpeg.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func alsPNG(t *testing.T, img image.Image) image.Image {
	var buf bytes.Buffer
	png.Encode(&buf, img)
	out, err := png.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fp(t *testing.T, img image.Image) ImageFP {
	t.Helper()
	f, ok := imageFingerprint(img)
	if !ok {
		t.Fatal("kein Fingerabdruck")
	}
	return f
}

func TestBildVariantenSindAehnlich(t *testing.T) {
	orig := testBild(1, 1200, 800)
	a := fp(t, orig)
	varianten := map[string]image.Image{
		"halbe Größe":      verkleinere(orig, 600, 400),
		"Miniatur 200px":   verkleinere(orig, 200, 133),
		"JPEG Qualität 40": alsJPEG(t, orig, 40),
		"PNG":              alsPNG(t, orig),
		"klein + JPEG q30": alsJPEG(t, verkleinere(orig, 400, 266), 30),
	}
	for name, v := range varianten {
		b := fp(t, v)
		if d := hamming(a.P, b.P); d > imgMaxP {
			t.Errorf("%s: pHash-Abstand %d > %d", name, d, imgMaxP)
		}
		if d := hamming(a.D, b.D); d > imgMaxD {
			t.Errorf("%s: dHash-Abstand %d > %d", name, d, imgMaxD)
		}
	}
}

func TestVerschiedeneBilderSindNichtAehnlich(t *testing.T) {
	a := fp(t, testBild(1, 800, 600))
	for seed := int64(2); seed < 30; seed++ {
		b := fp(t, testBild(seed, 800, 600))
		if hamming(a.P, b.P) <= imgMaxP && hamming(a.D, b.D) <= imgMaxD {
			t.Errorf("Bild %d gilt fälschlich als ähnlich (p=%d d=%d)", seed, hamming(a.P, b.P), hamming(a.D, b.D))
		}
	}
}

func TestEinfarbigesBildOhneFingerabdruck(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 300, 300))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	if _, ok := imageFingerprint(img); ok {
		t.Fatal("einfarbiges Bild darf keinen Fingerabdruck bekommen")
	}
}

const beispielText = `Der Duplikat-Finder vergleicht Dateien aus dem Netz und aus lokalen Ordnern.
Zuerst wird die Größe geprüft, danach Anfang und Ende, erst zum Schluss der vollständige Inhalt.
Für Bilder, Videos und Texte gibt es zusätzlich Fingerabdrücke, die auch dann gleich bleiben,
wenn eine Datei in einem anderen Format gespeichert, verkleinert oder neu komprimiert wurde.
So lassen sich Kopien finden, die Byte für Byte verschieden sind, inhaltlich aber dasselbe zeigen.
Das spart Speicherplatz, weil man überflüssige Varianten gezielt aussortieren kann.`

func TestTextVariantenSindAehnlich(t *testing.T) {
	a, ok := textFingerprint(beispielText)
	if !ok {
		t.Fatal("kein Text-Fingerabdruck")
	}
	varianten := map[string]string{
		"andere Formatierung": strings.ToUpper(strings.ReplaceAll(beispielText, "\n", "\n\n   ")),
		"Satzzeichen":         strings.NewReplacer(".", " ", ",", " ; ").Replace(beispielText),
		"ein Wort geändert":   strings.Replace(beispielText, "Speicherplatz", "Festplattenplatz", 1),
	}
	for name, v := range varianten {
		b, _ := textFingerprint(v)
		if d, j := hamming(a.Sim, b.Sim), jaccard(a.Min, b.Min); !textSimilar(d, j) {
			t.Errorf("%s: nicht ähnlich (SimHash %d Bit, Jaccard %.2f)", name, d, j)
		} else {
			t.Logf("%s: SimHash %d Bit, Jaccard %.2f", name, d, j)
		}
	}
	anders, _ := textFingerprint(strings.Repeat("Ein völlig anderer Text über Kochrezepte, Nudeln, Tomaten und Basilikum im Sommer. ", 6))
	if d, j := hamming(a.Sim, anders.Sim), jaccard(a.Min, anders.Min); textSimilar(d, j) {
		t.Errorf("anderer Text gilt als ähnlich (SimHash %d, Jaccard %.2f)", d, j)
	}
}

func TestKurzerTextOhneFingerabdruck(t *testing.T) {
	if _, ok := textFingerprint("nur ein paar Wörter"); ok {
		t.Fatal("zu kurzer Text darf keinen Fingerabdruck bekommen")
	}
}

func TestMIHFindetAlleBisSiebenBits(t *testing.T) {
	var m mih
	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 20000; i++ {
		m.add(mihEntry{fp: int64(i), h: rnd.Uint64()})
	}
	ziel := rnd.Uint64()
	for d := 0; d <= 7; d++ {
		h := ziel
		for _, b := range rnd.Perm(64)[:d] {
			h ^= 1 << uint(b)
		}
		m.add(mihEntry{fp: int64(100000 + d), h: h})
	}
	gef := map[int64]int{}
	m.near(ziel, 7, func(x mihEntry, d int) { gef[x.fp] = d })
	for d := 0; d <= 7; d++ {
		if got, ok := gef[int64(100000+d)]; !ok || got != d {
			t.Errorf("Eintrag mit Abstand %d nicht gefunden (gefunden: %v)", d, ok)
		}
	}
}

func TestLatin1WirdUTF8(t *testing.T) {
	if s := toUTF8([]byte{'M', 0xfc, 'l', 'l'}); s != "Müll" {
		t.Fatalf("got %q", s)
	}
}

func TestSchwarzWeissScanWirdErkannt(t *testing.T) {
	seite := image.NewGray(image.Rect(0, 0, 600, 800))
	rnd := rand.New(rand.NewSource(3))
	for i := range seite.Pix {
		seite.Pix[i] = 255
	}
	for z := 60; z < 740; z += 18 { // „Textzeilen“
		for x := 50; x < 550; x++ {
			if rnd.Intn(3) > 0 {
				for y := z; y < z+9; y++ {
					seite.Pix[y*600+x] = 0
				}
			}
		}
	}
	if !isBitonal(seite) {
		t.Error("eingescannte Textseite nicht als Schwarz-Weiß erkannt")
	}
	if isBitonal(testBild(1, 600, 400)) {
		t.Error("Farbbild fälschlich als Schwarz-Weiß erkannt")
	}
	if !isBitonal(alsJPEG(t, seite, 60)) {
		t.Error("JPEG-komprimierter Textscan nicht als Schwarz-Weiß erkannt")
	}
}

// Nachgestellt aus echten Daten (bitsavers): lange OCR-Texte verschiedener Handbücher mit gleichem Fachvokabular,
// Zahlenkolonnen und Zeichensalat dürfen NICHT als ähnlich gelten – Varianten desselben Textes schon.
func langerFachtext(seed int64, n int) string {
	rnd := rand.New(rand.NewSource(seed))
	vokabular := strings.Fields(`system unit processor storage channel data address register control signal circuit
		input output disk printer card reader punch diagnostic routine error check parity bit byte word field
		maintenance procedure step test switch panel indicator lamp console operator program instruction
		the of and to in for is on with by this that be are from as at or an it`)
	var b strings.Builder
	for i := 0; i < n; i++ {
		switch r := rnd.Intn(10); {
		case r < 7:
			b.WriteString(vokabular[rnd.Intn(len(vokabular))])
		case r < 9:
			fmt.Fprintf(&b, "%d", rnd.Intn(100)) // Tabellen, Seitenzahlen
		default:
			b.WriteString(string(rune('a' + rnd.Intn(26)))) // OCR-Zeichensalat
		}
		b.WriteByte(' ')
	}
	return b.String()
}

func TestLangeOCRTexteNichtAehnlich(t *testing.T) {
	a, _ := textFingerprint(langerFachtext(1, 120000))
	for seed := int64(2); seed <= 6; seed++ {
		b, _ := textFingerprint(langerFachtext(seed, 120000))
		t.Logf("Handbuch %d: SimHash %d Bit, Jaccard %.2f", seed, hamming(a.Sim, b.Sim), jaccard(a.Min, b.Min))
		if j := jaccard(a.Min, b.Min); textSimilar(hamming(a.Sim, b.Sim), j) {
			t.Errorf("verschiedene Handbücher %d gelten als ähnlich (SimHash %d Bit, Jaccard %.2f)", seed, hamming(a.Sim, b.Sim), j)
		}
	}
	// dieselbe Ausgabe mit 3 % geänderten Wörtern (andere Auflage/OCR) bleibt ähnlich
	w := strings.Fields(langerFachtext(1, 120000))
	rnd := rand.New(rand.NewSource(9))
	for i := 0; i < len(w)*3/100; i++ {
		w[rnd.Intn(len(w))] = "geändert"
	}
	v, _ := textFingerprint(strings.Join(w, " "))
	if j := jaccard(a.Min, v.Min); !textSimilar(hamming(a.Sim, v.Sim), j) {
		t.Errorf("Variante mit 3 %% Änderungen nicht erkannt (SimHash %d Bit, Jaccard %.2f)", hamming(a.Sim, v.Sim), j)
	}
}
