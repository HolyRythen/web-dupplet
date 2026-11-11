package main

import (
	"bytes"
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
		if d := hamming(a.Sim, b.Sim); d > txtMax {
			t.Errorf("%s: Abstand %d > %d", name, d, txtMax)
		}
	}
	anders, _ := textFingerprint(strings.Repeat("Ein völlig anderer Text über Kochrezepte, Nudeln, Tomaten und Basilikum im Sommer. ", 6))
	if d := hamming(a.Sim, anders.Sim); d <= txtMax {
		t.Errorf("anderer Text gilt als ähnlich (Abstand %d)", d)
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
