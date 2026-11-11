package main

// Fingerabdrücke für die Ähnlichkeitsanalyse (Stufe 4).
//
//   Bilder:  pHash (DCT über 32×32 Graustufen, 64 Bit) + dHash (Helligkeitsverlauf, 64 Bit).
//            Bleibt gleich bei anderer Auflösung, anderem Format, stärkerer Kompression.
//   Videos:  pHash von N Einzelbildern an festen relativen Positionen (5 %, 17,5 %, …).
//   Texte:   SimHash (64 Bit) über Wort-Dreiergruppen des normalisierten Textes.
//
// Ähnlichkeit = Anzahl unterschiedlicher Bits (Hamming-Abstand).

import (
	"bytes"
	"hash/fnv"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/bits"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const hashSide = 32 // Kantenlänge des Graustufenbildes für den pHash

// Schwellen (Hamming-Abstand in Bit von 64), ab denen zwei Inhalte als „ähnlich“ gelten.
const (
	imgMaxP   = 8  // Bilder: pHash
	imgMaxD   = 14 // Bilder: dHash als zweite Bestätigung (weniger Fehltreffer)
	txtMax    = 7  // Texte: SimHash (Zufallstexte liegen bei ≈32 ± 4 Bit; ≤7 findet der Index garantiert)
	vidFrameD = 10 // Videos: je Einzelbild
)

// dctCos[u][x] = cos((2x+1)·u·π / 64) für die ersten 8 Frequenzen
var dctCos = func() (t [8][hashSide]float64) {
	for u := 0; u < 8; u++ {
		for x := 0; x < hashSide; x++ {
			t[u][x] = math.Cos(float64(2*x+1) * float64(u) * math.Pi / (2 * hashSide))
		}
	}
	return
}()

// boxResize verkleinert ein Bild per Flächenmittelung auf w×h (eine einzige Runde über alle Pixel).
// Schnellwege für die häufigen Typen YCbCr (JPEG), Gray, RGBA und NRGBA.
func boxResize(img image.Image, w, h int) (r, g, b []float64) {
	bd := img.Bounds()
	W, H := bd.Dx(), bd.Dy()
	n := w * h
	r, g, b = make([]float64, n), make([]float64, n), make([]float64, n)
	cnt := make([]float64, n)
	if W <= 0 || H <= 0 {
		return
	}
	xm := make([]int, W)
	for x := 0; x < W; x++ {
		xm[x] = x * w / W
	}
	add := func(x, y int, rr, gg, bb float64) {
		i := (y*h/H)*w + xm[x]
		r[i] += rr
		g[i] += gg
		b[i] += bb
		cnt[i]++
	}
	switch m := img.(type) {
	case *image.YCbCr:
		for y := 0; y < H; y++ {
			for x := 0; x < W; x++ {
				yi := m.YOffset(bd.Min.X+x, bd.Min.Y+y)
				ci := m.COffset(bd.Min.X+x, bd.Min.Y+y)
				R, G, B := color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci])
				add(x, y, float64(R), float64(G), float64(B))
			}
		}
	case *image.Gray:
		for y := 0; y < H; y++ {
			row := m.Pix[y*m.Stride:]
			for x := 0; x < W; x++ {
				v := float64(row[x])
				add(x, y, v, v, v)
			}
		}
	case *image.RGBA:
		for y := 0; y < H; y++ {
			row := m.Pix[y*m.Stride:]
			for x := 0; x < W; x++ {
				p := row[4*x:]
				add(x, y, float64(p[0]), float64(p[1]), float64(p[2]))
			}
		}
	case *image.NRGBA:
		for y := 0; y < H; y++ {
			row := m.Pix[y*m.Stride:]
			for x := 0; x < W; x++ {
				p := row[4*x:]
				a := float64(p[3]) / 255 // transparente Pixel wie auf weißem Grund
				add(x, y, float64(p[0])*a+255*(1-a), float64(p[1])*a+255*(1-a), float64(p[2])*a+255*(1-a))
			}
		}
	default:
		for y := 0; y < H; y++ {
			for x := 0; x < W; x++ {
				R, G, B, A := img.At(bd.Min.X+x, bd.Min.Y+y).RGBA()
				a := float64(A) / 0xffff
				add(x, y, float64(R>>8)+255*(1-a), float64(G>>8)+255*(1-a), float64(B>>8)+255*(1-a))
			}
		}
	}
	for i := range cnt {
		if c := cnt[i]; c > 0 {
			r[i] /= c
			g[i] /= c
			b[i] /= c
		}
	}
	return
}

func luma(r, g, b []float64) []float64 {
	out := make([]float64, len(r))
	for i := range r {
		out[i] = 0.299*r[i] + 0.587*g[i] + 0.114*b[i]
	}
	return out
}

// pHashGray berechnet den pHash aus 32×32 Graustufen (Zeilen hintereinander).
// ok=false bei (fast) einfarbigen Bildern: die wären sonst mit jedem anderen einfarbigen Bild „ähnlich“.
func pHashGray(px []float64) (h uint64, ok bool) {
	if len(px) != hashSide*hashSide {
		return 0, false
	}
	var mean, sq float64
	for _, v := range px {
		mean += v
	}
	mean /= float64(len(px))
	for _, v := range px {
		sq += (v - mean) * (v - mean)
	}
	if math.Sqrt(sq/float64(len(px))) < 3 {
		return 0, false
	}
	// nur die 8×8 niedrigsten Frequenzen der 2D-DCT
	var tmp [8][hashSide]float64 // tmp[u][y] = Σx px[y][x]·cos_u(x)
	for u := 0; u < 8; u++ {
		for y := 0; y < hashSide; y++ {
			var s float64
			row := px[y*hashSide : (y+1)*hashSide]
			for x, v := range row {
				s += v * dctCos[u][x]
			}
			tmp[u][y] = s
		}
	}
	var c [64]float64
	for v := 0; v < 8; v++ {
		for u := 0; u < 8; u++ {
			var s float64
			for y := 0; y < hashSide; y++ {
				s += tmp[u][y] * dctCos[v][y]
			}
			c[v*8+u] = s
		}
	}
	ac := make([]float64, 0, 63)
	ac = append(ac, c[1:]...) // Gleichanteil (Helligkeit) nicht für den Median
	sort.Float64s(ac)
	med := (ac[31] + ac[32]) / 2
	for i, v := range c {
		if v > med {
			h |= 1 << uint(i)
		}
	}
	return h, true
}

// dHashGray: 9×8 Graustufen → 64 Bit „links heller als rechts“.
func dHashGray(px []float64) uint64 {
	var h uint64
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if px[y*9+x] > px[y*9+x+1] {
				h |= 1 << uint(y*8+x)
			}
		}
	}
	return h
}

// ImageFP ist der Fingerabdruck eines Bildes.
type ImageFP struct {
	P, D  uint64 // pHash, dHash
	W, H  int
	Thumb []byte // kleine JPEG-Vorschau (max. 160 px)
}

// imageFingerprint: ok=false bei einfarbigen/leeren Bildern.
func imageFingerprint(img image.Image) (fp ImageFP, ok bool) {
	b := img.Bounds()
	fp.W, fp.H = b.Dx(), b.Dy()
	if fp.W < 8 || fp.H < 8 {
		return fp, false
	}
	r, g, bl := boxResize(img, hashSide, hashSide)
	if fp.P, ok = pHashGray(luma(r, g, bl)); !ok {
		return fp, false
	}
	r, g, bl = boxResize(img, 9, 8)
	fp.D = dHashGray(luma(r, g, bl))
	fp.Thumb = thumbnail(img, 160)
	return fp, true
}

// thumbnail erzeugt eine kleine JPEG-Vorschau (längste Seite = max Pixel).
func thumbnail(img image.Image, maxSide int) []byte {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	tw, th := w, h
	if w > maxSide || h > maxSide {
		if w >= h {
			tw, th = maxSide, max(1, h*maxSide/w)
		} else {
			tw, th = max(1, w*maxSide/h), maxSide
		}
	}
	r, g, bl := boxResize(img, tw, th)
	out := image.NewRGBA(image.Rect(0, 0, tw, th))
	for i := range r {
		out.Pix[4*i], out.Pix[4*i+1], out.Pix[4*i+2], out.Pix[4*i+3] =
			uint8(r[i]+.5), uint8(g[i]+.5), uint8(bl[i]+.5), 255
	}
	var buf bytes.Buffer
	if jpeg.Encode(&buf, out, &jpeg.Options{Quality: 72}) != nil {
		return nil
	}
	return buf.Bytes()
}

func hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// ---------- Text ----------

// normWords: Kleinbuchstaben, nur Buchstaben/Ziffern, Umlaute bleiben erhalten.
func normWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// minTextWords: kürzere Texte sind zu unspezifisch (Lizenzhinweise, Leerseiten …).
const minTextWords = 30

// TextFP ist der Fingerabdruck eines Textes.
type TextFP struct {
	Sim   uint64 // SimHash
	Words int
	Snip  string // Textanfang für die Vorschau
}

func textFingerprint(s string) (fp TextFP, ok bool) {
	w := normWords(s)
	fp.Words = len(w)
	if len(w) < minTextWords {
		return fp, false
	}
	var v [64]int
	h := fnv.New64a()
	for i := 0; i+3 <= len(w); i++ {
		h.Reset()
		h.Write([]byte(w[i]))
		h.Write([]byte{0})
		h.Write([]byte(w[i+1]))
		h.Write([]byte{0})
		h.Write([]byte(w[i+2]))
		x := h.Sum64()
		for b := 0; b < 64; b++ {
			if x&(1<<uint(b)) != 0 {
				v[b]++
			} else {
				v[b]--
			}
		}
	}
	for b := 0; b < 64; b++ {
		if v[b] > 0 {
			fp.Sim |= 1 << uint(b)
		}
	}
	fp.Snip = snippet(s, 280)
	return fp, true
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + " …"
}

// toUTF8: gültiges UTF-8 bleibt, sonst als Latin-1 (Windows-1252-nah) deuten.
func toUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// ---------- Index für schnelle Ähnlichkeitssuche ----------
//
// Multi-Index-Hashing: Der 64-Bit-Hash wird in 8 Bytes zerlegt. Unterscheiden sich zwei Hashes
// in höchstens 7 Bits, stimmt mindestens eines der 8 Bytes exakt überein (Schubfachprinzip).
// Pro Suche werden also nur die Einträge mit einem gleichen Byte geprüft (≈ 8/256 aller Einträge).

type mihEntry struct {
	fp  int64  // fps.id
	h   uint64 // Hash
	aux uint64 // Zweit-Hash (dHash) bzw. Bildnummer bei Videos
}

type mih struct {
	e   []mihEntry
	bkt [8][256][]int32
}

func (m *mih) add(x mihEntry) {
	i := int32(len(m.e))
	m.e = append(m.e, x)
	for k := 0; k < 8; k++ {
		b := uint8(x.h >> (8 * k))
		m.bkt[k][b] = append(m.bkt[k][b], i)
	}
}

// near ruft fn für jeden Eintrag mit Hamming-Abstand ≤ maxd auf (jeden höchstens einmal).
// Abstände ≤ 7 werden garantiert gefunden, 8 und mehr nur, wenn ein Byte zufällig passt.
func (m *mih) near(h uint64, maxd int, fn func(x mihEntry, d int)) {
	seen := map[int32]struct{}{}
	for k := 0; k < 8; k++ {
		for _, i := range m.bkt[k][uint8(h>>(8*k))] {
			if _, ok := seen[i]; ok {
				continue
			}
			seen[i] = struct{}{}
			if d := hamming(h, m.e[i].h); d <= maxd {
				fn(m.e[i], d)
			}
		}
	}
}
