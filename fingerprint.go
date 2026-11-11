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
	"encoding/binary"
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
	txtMax    = 14 // Texte: SimHash-Vorfilter (großzügig) – entschieden wird über MinHash (txtMinJac)
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

// isBitonal erkennt Schwarz-Weiß-Bilder (typisch: eingescannte Textseiten in PDFs).
// Solche Seiten sehen sich verkleinert alle ähnlich und würden massenhaft falsche Treffer erzeugen.
func isBitonal(img image.Image) bool {
	switch m := img.(type) {
	case *image.Paletted:
		return len(m.Palette) <= 2
	}
	b := img.Bounds()
	step := max(1, int(math.Sqrt(float64(b.Dx()*b.Dy())/20000))) // ≈ 20.000 Stichproben
	var n, extreme int
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, _ := img.At(x, y).RGBA()
			l := (299*r + 587*g + 114*bl) / 1000 >> 8
			n++
			if l < 40 || l > 215 {
				extreme++
			}
		}
	}
	return n > 0 && float64(extreme) >= 0.97*float64(n)
}

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
//
// Sim: SimHash (64 Bit) – schneller Vorfilter über den Such-Index.
// Min: MinHash (64 Werte) – schätzt, welcher Anteil der Wort-Dreiergruppen beiden Texten gemeinsam ist (Jaccard).
// Erst beides zusammen ist zuverlässig: Bei sehr langen Texten (z.B. OCR-Handbücher mit 100.000+ Wörtern und
// gleichem Fachvokabular) gleichen sich SimHash-Werte an; MinHash unterscheidet sie sicher.
type TextFP struct {
	Sim   uint64
	Min   []uint32
	Words int
	Snip  string // Textanfang für die Vorschau
}

const (
	minHashN  = 64
	txtMinJac = 0.5 // mindestens die Hälfte der Wort-Dreiergruppen gemeinsam
)

var minHashSeeds = func() (s [minHashN]uint64) {
	x := uint64(0x9e3779b97f4a7c15)
	for i := range s {
		x = splitmix(x)
		s[i] = x
	}
	return
}()

func splitmix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// shingleWord: Zahlen und Einzelzeichen (Seitenzahlen, Tabellen, OCR-Zeichensalat) tragen nichts zum Inhalt bei.
func shingleWord(w string) bool {
	if utf8.RuneCountInString(w) < 2 {
		return false
	}
	for _, r := range w {
		if !unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func textFingerprint(s string) (fp TextFP, ok bool) {
	all := normWords(s)
	fp.Words = len(all)
	if len(all) < minTextWords {
		return fp, false
	}
	w := all[:0:0]
	for _, x := range all {
		if shingleWord(x) {
			w = append(w, x)
		}
	}
	if len(w) < minTextWords {
		return fp, false
	}
	// jede Dreiergruppe nur einmal (Menge) – häufige Wendungen und Tabellenzeilen dominieren sonst
	set := make(map[uint64]struct{}, len(w))
	h := fnv.New64a()
	for i := 0; i+3 <= len(w); i++ {
		h.Reset()
		h.Write([]byte(w[i]))
		h.Write([]byte{0})
		h.Write([]byte(w[i+1]))
		h.Write([]byte{0})
		h.Write([]byte(w[i+2]))
		set[h.Sum64()] = struct{}{}
	}
	var v [64]int
	mins := make([]uint64, minHashN)
	for i := range mins {
		mins[i] = ^uint64(0)
	}
	for x := range set {
		for b := 0; b < 64; b++ {
			if x&(1<<uint(b)) != 0 {
				v[b]++
			} else {
				v[b]--
			}
		}
		for i, seed := range minHashSeeds {
			if m := splitmix(x ^ seed); m < mins[i] {
				mins[i] = m
			}
		}
	}
	for b := 0; b < 64; b++ {
		if v[b] > 0 {
			fp.Sim |= 1 << uint(b)
		}
	}
	fp.Min = make([]uint32, minHashN)
	for i, m := range mins {
		fp.Min[i] = uint32(m >> 32)
	}
	fp.Snip = snippet(s, 280)
	return fp, true
}

// jaccard schätzt den Anteil gemeinsamer Wort-Dreiergruppen (0–1). Ohne MinHash (alte Daten): 0.
func jaccard(a, b []uint32) float64 {
	if len(a) != minHashN || len(b) != minHashN {
		return 0
	}
	n := 0
	for i := range a {
		if a[i] == b[i] {
			n++
		}
	}
	return float64(n) / minHashN
}

// textSimilar: SimHash-Vorfilter UND MinHash-Bestätigung.
func textSimilar(simDist int, jac float64) bool { return simDist <= txtMax && jac >= txtMinJac }

func minBlob(m []uint32) []byte {
	b := make([]byte, 4*len(m))
	for i, x := range m {
		binary.LittleEndian.PutUint32(b[4*i:], x)
	}
	return b
}

func blobMin(b []byte) []uint32 {
	if len(b) != 4*minHashN {
		return nil
	}
	m := make([]uint32, minHashN)
	for i := range m {
		m[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return m
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
	fp   int64  // fps.id
	file int64  // files.id (Treffer in derselben Datei werden ignoriert)
	h    uint64 // Hash
	aux  uint64 // Zweit-Hash (dHash) bzw. Bildnummer bei Videos
	cs   uint64 // Anfang des SHA-256 der Bytes (0 = unbekannt)
	ex   bool   // Byte-Gleichheit wird schon anderswo gemeldet (ganze Datei oder Archiv-Eintrag)
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
