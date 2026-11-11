package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errPermanent: kein erneuter Versuch sinnvoll (404, 403, ...)
type errPermanent struct{ error }

// errChanged: Datei hat sich seit der Größenprüfung verändert → neu prüfen
var errChanged = errors.New("Inhalt/Größe hat sich geändert")

var bufPool = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// ---------- Teil-Hash ----------
// phash = SHA256( size(8 Byte LE) || erste C Bytes || letzte C Bytes )
// Identisch, egal ob per Range-Request oder beim vollständigen Streamen berechnet.

func partialHash(size int64, head, tail []byte) []byte {
	h := sha256.New()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(size))
	h.Write(b[:])
	h.Write(head)
	h.Write(tail)
	return h.Sum(nil)
}

// capture sammelt beim Streamen Anfang und Ende, zählt Bytes.
type capture struct {
	c    int
	n    int64
	head []byte
	tail []byte
}

func newCapture(c int) *capture {
	return &capture{c: c, head: make([]byte, 0, c), tail: make([]byte, 0, 2*c)}
}

func (k *capture) Write(p []byte) (int, error) {
	k.n += int64(len(p))
	if len(k.head) < k.c {
		n := min(k.c-len(k.head), len(p))
		k.head = append(k.head, p[:n]...)
	}
	if len(p) >= k.c {
		k.tail = append(k.tail[:0], p[len(p)-k.c:]...)
	} else {
		if len(k.tail)+len(p) > cap(k.tail) {
			keep := min(k.c, len(k.tail))
			copy(k.tail, k.tail[len(k.tail)-keep:])
			k.tail = k.tail[:keep]
		}
		k.tail = append(k.tail, p...)
	}
	return len(p), nil
}

func (k *capture) phash() []byte {
	t := k.tail
	if len(t) > k.c {
		t = t[len(t)-k.c:]
	}
	return partialHash(k.n, k.head, t)
}

// ---------- Fetcher ----------

type Fetcher struct {
	client  *http.Client
	probeC  *http.Client // eigenes, höheres Verbindungslimit für billige Größenabfragen
	chunk   int
	ua      string
	stall   time.Duration
	store   string // leer = nichts speichern
	bytes   *atomic.Int64
	storeMu sync.Mutex
	multi   sync.Map     // Host → unterstützt Multi-Range?
	lim     *rateLimiter // -max-rate (nil = unbegrenzt)
}

func newFetcher(cfg *Config, bytes *atomic.Int64) *Fetcher {
	tr := &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		// HTTP/1.1 mit mehreren echten Verbindungen: bei HTTP/2 stauen sich tausende
		// kleine Range-Anfragen in einer einzigen Verbindung (gemessen).
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConns:          cfg.ProbeWorkers + cfg.FetchWorkers + 16,
		MaxIdleConnsPerHost:   cfg.PerHost,
		MaxConnsPerHost:       cfg.PerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: cfg.Stall,
		DisableCompression:    true,
		ReadBufferSize:        256 << 10,
	}
	ptr := tr.Clone()
	ptr.MaxConnsPerHost = cfg.ProbePerHost
	ptr.MaxIdleConnsPerHost = cfg.ProbePerHost
	return &Fetcher{
		client: &http.Client{Transport: tr},
		probeC: &http.Client{Transport: ptr},
		chunk:  cfg.Chunk, ua: cfg.UserAgent, stall: cfg.Stall, store: cfg.Store, bytes: bytes,
		lim: newRateLimiter(cfg.MaxRateMB),
	}
}

func (f *Fetcher) req(ctx context.Context, method, url, rng string) (*http.Response, error) {
	rq, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, errPermanent{err}
	}
	rq.Header.Set("User-Agent", f.ua)
	rq.Header.Set("Accept-Encoding", "identity")
	if rng != "" {
		rq.Header.Set("Range", rng)
	}
	c := f.client
	if method == http.MethodHead || rng == "bytes=0-0" {
		c = f.probeC // Größenabfrage: kaum Last für den Server, nur Wartezeit
	}
	resp, err := c.Do(rq)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 404 || resp.StatusCode == 410 || resp.StatusCode == 401 ||
		resp.StatusCode == 403 || resp.StatusCode == 451:
		resp.Body.Close()
		return nil, errPermanent{fmt.Errorf("HTTP %d", resp.StatusCode)}
	case resp.StatusCode >= 400:
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// totalFromContentRange: "bytes 0-0/12345" → 12345 ("*" → -1)
func totalFromContentRange(s string) int64 {
	i := strings.LastIndexByte(s, '/')
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s[i+1:]), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// Probe ermittelt nur die Größe – kostet praktisch keine Bandbreite.
func (f *Fetcher) Probe(ctx context.Context, url string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*f.stall)
	defer cancel()
	if resp, err := f.req(ctx, http.MethodHead, url, ""); err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 && resp.ContentLength >= 0 && resp.Header.Get("Content-Encoding") == "" {
			return resp.ContentLength, nil
		}
	} else if ctx.Err() != nil {
		return 0, err
	} else if errors.As(err, new(errPermanent)) && !strings.Contains(err.Error(), "403") {
		return 0, err
	}
	// Fallback: GET mit Range 0-0 (manche Server verbieten HEAD)
	resp, err := f.req(ctx, http.MethodGet, url, "bytes=0-0")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPartialContent {
		return totalFromContentRange(resp.Header.Get("Content-Range")), nil
	}
	return resp.ContentLength, nil // -1 wenn unbekannt
}

// rateLimiter begrenzt die gesamte Download-Rate (Token-Bucket, -max-rate). nil = unbegrenzt.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // Bytes pro Sekunde
	tokens float64
	last   time.Time
}

func newRateLimiter(mbPerSec float64) *rateLimiter {
	if mbPerSec <= 0 {
		return nil
	}
	r := mbPerSec * 1e6
	return &rateLimiter{rate: r, tokens: r / 4, last: time.Now()}
}

// take verbucht n Bytes und wartet, falls das Budget überzogen ist.
func (l *rateLimiter) take(n int) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	now := time.Now()
	l.tokens = min(l.rate/4, l.tokens+now.Sub(l.last).Seconds()*l.rate) // höchstens 250 ms Vorrat
	l.last = now
	l.tokens -= float64(n)
	var wait time.Duration
	if l.tokens < 0 {
		wait = time.Duration(-l.tokens / l.rate * float64(time.Second))
	}
	l.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}

// stallReader bricht ab, wenn 'stall' lang keine Daten kommen, und hält die Ratenbegrenzung ein.
type stallReader struct {
	r   io.Reader
	t   *time.Timer
	d   time.Duration
	b   *atomic.Int64
	lim *rateLimiter
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.lim.take(n)
		s.t.Reset(s.d)
		s.b.Add(int64(n))
	}
	return n, err
}

func (f *Fetcher) readRange(ctx context.Context, url string, from, to, size int64) ([]byte, *http.Response, error) {
	resp, err := f.req(ctx, http.MethodGet, url, fmt.Sprintf("bytes=%d-%d", from, to))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, resp, nil // Server kann keine Ranges → Aufrufer streamt komplett
	}
	defer resp.Body.Close()
	if t := totalFromContentRange(resp.Header.Get("Content-Range")); t >= 0 && t != size {
		return nil, nil, errChanged
	}
	buf := make([]byte, to-from+1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		return nil, nil, err
	}
	f.lim.take(len(buf))
	f.bytes.Add(int64(len(buf)))
	return buf, nil, nil
}

func hostOf(u string) string {
	if p, err := neturl.Parse(u); err == nil {
		return p.Host
	}
	return u
}

// readMulti fragt "bytes=0-(c-1),(size-c)-(size-1)" an.
// ok=false: Server kann kein Multi-Range (dann klassisch mit 2 Anfragen).
func (f *Fetcher) readMulti(ctx context.Context, url string, c, size int64) (head, tail []byte, full *http.Response, ok bool, err error) {
	resp, err := f.req(ctx, http.MethodGet, url, fmt.Sprintf("bytes=0-%d,%d-%d", c-1, size-c, size-1))
	if err != nil {
		return nil, nil, nil, false, err
	}
	if resp.StatusCode == http.StatusOK {
		return nil, nil, resp, false, nil
	}
	defer resp.Body.Close()
	mt, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusPartialContent || mt != "multipart/byteranges" {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 2*c))
		return nil, nil, nil, false, nil
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return nil, nil, nil, false, perr
		}
		cr := part.Header.Get("Content-Range")
		if t := totalFromContentRange(cr); t >= 0 && t != size {
			return nil, nil, nil, false, errChanged
		}
		var from, to int64
		if _, e := fmt.Sscanf(cr, "bytes %d-%d/", &from, &to); e != nil || to-from+1 != c {
			return nil, nil, nil, false, nil
		}
		buf := make([]byte, c)
		if _, e := io.ReadFull(part, buf); e != nil {
			return nil, nil, nil, false, e
		}
		f.lim.take(int(c))
		f.bytes.Add(c)
		switch from {
		case 0:
			head = buf
		case size - c:
			tail = buf
		}
	}
	return head, tail, nil, head != nil && tail != nil, nil
}

// PartialHTTP lädt nur Anfang und Ende. Liefert full != nil, falls dabei
// zwangsläufig alles geladen wurde (kleine Datei / Server ohne Range).
func (f *Fetcher) PartialHTTP(ctx context.Context, url string, size int64) (ph, fh []byte, n int64, err error) {
	c := int64(f.chunk)
	if size <= 2*c {
		return f.FullHTTP(ctx, url, size)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := time.AfterFunc(4*f.stall, cancel) // Range-Anfragen sind klein → kurzes Limit

	// Schnellweg: Anfang + Ende in EINER Anfrage (multipart/byteranges) – halbiert die Wartezeit.
	host := hostOf(url)
	if v, ok := f.multi.Load(host); !ok || v.(bool) {
		head, tail, resp, ok2, err := f.readMulti(ctx, url, c, size)
		if err != nil {
			t.Stop()
			return nil, nil, 0, err
		}
		if resp != nil { // Server ignoriert Ranges → komplett streamen
			t.Stop()
			return f.streamFull(ctx, resp, size)
		}
		f.multi.Store(host, ok2)
		if ok2 {
			t.Stop()
			return partialHash(size, head, tail), nil, size, nil
		}
	}

	head, resp, err := f.readRange(ctx, url, 0, c-1, size)
	if err != nil {
		return nil, nil, 0, err
	}
	if resp != nil {
		t.Stop()
		return f.streamFull(ctx, resp, size)
	}
	tail, resp, err := f.readRange(ctx, url, size-c, size-1, size)
	t.Stop()
	if err != nil {
		return nil, nil, 0, err
	}
	if resp != nil {
		return f.streamFull(ctx, resp, size)
	}
	return partialHash(size, head, tail), nil, size, nil
}

func (f *Fetcher) FullHTTP(ctx context.Context, url string, size int64) (ph, fh []byte, n int64, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := f.req(ctx, http.MethodGet, url, "")
	if err != nil {
		return nil, nil, 0, err
	}
	return f.streamFull(ctx, resp, size)
}

func (f *Fetcher) streamFull(ctx context.Context, resp *http.Response, size int64) (ph, fh []byte, n int64, err error) {
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil, 0, fmt.Errorf("unerwarteter Status %d", resp.StatusCode)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	t := time.AfterFunc(f.stall, cancel)
	defer t.Stop()
	body := &stallReader{r: resp.Body, t: t, d: f.stall, b: f.bytes, lim: f.lim}

	var tmp *os.File
	if f.store != "" {
		if tmp, err = os.CreateTemp(filepath.Join(f.store, "tmp"), "dl-*"); err != nil {
			return
		}
		defer func() {
			if tmp != nil {
				tmp.Close()
				os.Remove(tmp.Name())
			}
		}()
	}
	h := sha256.New()
	k := newCapture(f.chunk)
	if err = copyHash(body, h, k, tmp); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("Download abgebrochen (>%s ohne Daten oder Programmende)", f.stall)
		}
		return
	}
	if size >= 0 && k.n != size {
		return nil, nil, 0, errChanged
	}
	fh = h.Sum(nil)
	if tmp != nil {
		if err = f.commitStore(tmp, fh); err != nil {
			return
		}
		tmp = nil
	}
	return k.phash(), fh, k.n, nil
}

// commitStore legt den Inhalt inhaltsadressiert ab: Duplikate belegen nur 1x Platz.
func (f *Fetcher) commitStore(tmp *os.File, fh []byte) error {
	if err := tmp.Close(); err != nil {
		return err
	}
	x := hex.EncodeToString(fh)
	dir := filepath.Join(f.store, "objects", x[:2], x[2:4])
	dst := filepath.Join(dir, x)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f.storeMu.Lock()
	defer f.storeMu.Unlock()
	if _, err := os.Stat(dst); err == nil {
		return os.Remove(tmp.Name()) // schon vorhanden → Duplikat nicht speichern
	}
	return os.Rename(tmp.Name(), dst)
}

func copyHash(r io.Reader, h hash.Hash, k *capture, file *os.File) error {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		n, err := r.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			k.Write(buf[:n])
			if file != nil {
				if _, werr := file.Write(buf[:n]); werr != nil {
					return werr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ---------- Lokale Dateien ----------

func (f *Fetcher) PartialLocal(path string, size int64, bytes *atomic.Int64) (ph, fh []byte, n int64, err error) {
	c := int64(f.chunk)
	if size <= 2*c {
		return f.FullLocal(path, size, bytes)
	}
	fd, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, errPermanent{err}
	}
	defer fd.Close()
	if st, e := fd.Stat(); e != nil || st.Size() != size {
		return nil, nil, 0, errChanged
	}
	head := make([]byte, c)
	tail := make([]byte, c)
	if _, err = fd.ReadAt(head, 0); err != nil {
		return
	}
	if _, err = fd.ReadAt(tail, size-c); err != nil {
		return
	}
	bytes.Add(2 * c)
	return partialHash(size, head, tail), nil, size, nil
}

func (f *Fetcher) FullLocal(path string, size int64, bytes *atomic.Int64) (ph, fh []byte, n int64, err error) {
	fd, err := os.Open(path)
	if err != nil {
		return nil, nil, 0, errPermanent{err}
	}
	defer fd.Close()
	h := sha256.New()
	k := newCapture(f.chunk)
	if err = copyHash(&countReader{fd, bytes}, h, k, nil); err != nil {
		return
	}
	if size >= 0 && k.n != size {
		return nil, nil, 0, errChanged
	}
	return k.phash(), h.Sum(nil), k.n, nil
}

type countReader struct {
	r io.Reader
	b *atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.b.Add(int64(n))
	return n, err
}
