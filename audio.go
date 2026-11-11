package main

// Audio-Fingerabdruck: N Ausschnitte (je bis zu 4 s) an festen relativen Positionen, von ffmpeg als
// Mono mit 5512 Hz dekodiert. Pro Ausschnitt: gemitteltes Spektrum (Hann-Fenster, FFT 1024) in
// 65 logarithmisch verteilten Bändern zwischen 250 und 2500 Hz → 64 Bit = „Band i lauter als Band i+1“.
// Die spektrale Hüllkurve bleibt bei MP3/OGG/Opus/AAC in verschiedenen Bitraten weitgehend erhalten.

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/cmplx"
	"strconv"
	"strings"
	"time"
)

const (
	audioSR    = 5512
	audioSeg   = 4.0 // Sekunden pro Ausschnitt
	audioFFT   = 1024
	audioBands = 65
	audFrameD  = 12 // Schwelle je Ausschnitt (Bit von 64)
)

var hannWin = func() []float64 {
	w := make([]float64, audioFFT)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(audioFFT-1))
	}
	return w
}()

// bandEdges[k] = erster FFT-Bin von Band k (logarithmisch 250–2500 Hz)
var bandEdges = func() []int {
	e := make([]int, audioBands+1)
	lo, hi := math.Log(250), math.Log(2500)
	for k := 0; k <= audioBands; k++ {
		f := math.Exp(lo + (hi-lo)*float64(k)/audioBands)
		e[k] = int(f * audioFFT / audioSR)
	}
	for k := 1; k <= audioBands; k++ { // jedes Band mindestens ein Bin breit
		if e[k] <= e[k-1] {
			e[k] = e[k-1] + 1
		}
	}
	return e
}()

// fft: einfache rekursive Radix-2-FFT (Länge = Zweierpotenz).
func fft(x []complex128) []complex128 {
	n := len(x)
	if n == 1 {
		return []complex128{x[0]}
	}
	even, odd := make([]complex128, n/2), make([]complex128, n/2)
	for i := 0; i < n/2; i++ {
		even[i], odd[i] = x[2*i], x[2*i+1]
	}
	e, o := fft(even), fft(odd)
	out := make([]complex128, n)
	for k := 0; k < n/2; k++ {
		t := cmplx.Rect(1, -2*math.Pi*float64(k)/float64(n)) * o[k]
		out[k], out[k+n/2] = e[k]+t, e[k]-t
	}
	return out
}

// audioSegHash: ok=false bei Stille oder zu kurzem Ausschnitt.
func audioSegHash(s []int16) (uint64, bool) {
	hop := audioFFT / 2
	if len(s) < audioFFT+3*hop {
		return 0, false
	}
	var rms float64
	for _, v := range s {
		rms += float64(v) * float64(v)
	}
	if math.Sqrt(rms/float64(len(s))) < 60 { // praktisch Stille
		return 0, false
	}
	var band [audioBands]float64
	buf := make([]complex128, audioFFT)
	frames := 0
	for off := 0; off+audioFFT <= len(s); off += hop {
		for i := 0; i < audioFFT; i++ {
			buf[i] = complex(float64(s[off+i])*hannWin[i], 0)
		}
		spec := fft(buf)
		for k := 0; k < audioBands; k++ {
			var e float64
			for b := bandEdges[k]; b < bandEdges[k+1]; b++ {
				m := cmplx.Abs(spec[b])
				e += m * m
			}
			band[k] += e / float64(bandEdges[k+1]-bandEdges[k])
		}
		frames++
	}
	var h uint64
	for k := 0; k < 64; k++ {
		if math.Log(1e-9+band[k]) > math.Log(1e-9+band[k+1]) {
			h |= 1 << uint(k)
		}
	}
	return h, true
}

// AudioFP: Hash je Ausschnitt (0 = Stille/unbrauchbar) + Dauer.
type AudioFP struct {
	Segs []uint64
	Dur  float64
}

func (t tools) audioFingerprint(ctx context.Context, src, ua string, n int) (fp AudioFP, err error) {
	if t.ffmpeg == "" || t.ffprobe == "" {
		return fp, errors.New("ffmpeg/ffprobe fehlt")
	}
	isURL := strings.Contains(src, "://")
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
		return fp, errors.New("Audiolänge unbekannt")
	}
	seg := min(audioSeg, fp.Dur/float64(n))
	if seg < 1 {
		return fp, errors.New("Audio zu kurz")
	}
	valid := 0
	for i := 0; i < n; i++ {
		ts := max(0, fp.Dur*(float64(i)+0.5)/float64(n)-seg/2)
		a := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
		if isURL {
			a = append(a, "-user_agent", ua, "-rw_timeout", "30000000")
		}
		a = append(a, "-ss", strconv.FormatFloat(ts, 'f', 3, 64), "-t", strconv.FormatFloat(seg, 'f', 3, 64),
			"-i", src, "-vn", "-ac", "1", "-ar", strconv.Itoa(audioSR), "-f", "s16le", "-")
		raw, err := runTool(ctx, 90*time.Second, t.ffmpeg, a...)
		var h uint64
		if err == nil {
			s := make([]int16, len(raw)/2)
			for k := range s {
				s[k] = int16(binary.LittleEndian.Uint16(raw[2*k:]))
			}
			var ok bool
			if h, ok = audioSegHash(s); ok {
				valid++
			}
		} else if ctx.Err() != nil {
			return fp, err
		}
		fp.Segs = append(fp.Segs, h)
	}
	if valid < 2 {
		return fp, errors.New("zu wenige auswertbare Ausschnitte (Stille?)")
	}
	return fp, nil
}

// seqScore vergleicht zwei Folgen von Ausschnitt-Hashes (Video-Einzelbilder bzw. Audio-Ausschnitte).
// ok, wenn ≥ 75 % der gemeinsam auswertbaren Ausschnitte höchstens thr Bit abweichen
// und die Länge um höchstens 2 % (mindestens 1 s) abweicht.
func seqScore(a, b []uint64, da, db float64, thr int) (score float64, ok bool) {
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
		if d <= thr {
			good++
		}
	}
	if n < 2 || good*4 < n*3 {
		return 0, false
	}
	return 1 - float64(sum)/float64(n)/64, true
}
