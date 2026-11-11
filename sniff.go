package main

// Dateityp am Inhalt erkennen (für Dateien ohne bzw. mit unbekannter Endung).

import (
	"bytes"
	"net/http"
	"strings"
)

const sniffLen = 4096

// sniff liefert Medientyp und eine passende Endung (für die Weiterverarbeitung) anhand der ersten Bytes.
// ctype: Content-Type des Servers (darf leer sein).
func sniff(b []byte, ctype string) (int, string) {
	has := func(off int, sig string) bool { return len(b) >= off+len(sig) && string(b[off:off+len(sig)]) == sig }
	switch {
	case has(0, "\xff\xd8\xff"):
		return mtImage, "jpg"
	case has(0, "\x89PNG\r\n\x1a\n"):
		return mtImage, "png"
	case has(0, "GIF87a"), has(0, "GIF89a"):
		return mtImage, "gif"
	case has(0, "RIFF") && has(8, "WEBP"):
		return mtImage, "webp"
	case has(0, "II*\x00"), has(0, "MM\x00*"):
		return mtImage, "tif"
	case has(0, "BM") && len(b) > 14 && b[14] >= 12 && b[14] <= 124: // BMP: Größe des Info-Headers plausibel
		return mtImage, "bmp"
	case has(0, "%PDF-"):
		return mtPDF, "pdf"
	case has(0, "PK\x03\x04"):
		return sniffZip(b)
	case has(0, "\x1f\x8b"):
		return mtArchive, "tgz"
	case has(0, "BZh"):
		return mtArchive, "tbz2"
	case has(257, "ustar"):
		return mtArchive, "tar"
	case has(4, "ftyp"):
		if has(8, "M4A ") || has(8, "M4B ") || has(8, "mp42") && bytes.Contains(b[:min(len(b), 64)], []byte("M4A ")) {
			return mtAudio, "m4a"
		}
		return mtVideo, "mp4"
	case has(0, "\x1a\x45\xdf\xa3"):
		return mtVideo, "mkv"
	case has(0, "RIFF") && has(8, "AVI "):
		return mtVideo, "avi"
	case has(0, "RIFF") && has(8, "WAVE"):
		return mtAudio, "wav"
	case has(0, "\x30\x26\xb2\x75\x8e\x66\xcf\x11"): // ASF (WMV/WMA)
		if bytes.Contains(b, []byte("W\x00i\x00n\x00d\x00o\x00w\x00s\x00 \x00M\x00e\x00d\x00i\x00a\x00 \x00A\x00u\x00d\x00i\x00o")) {
			return mtAudio, "wma"
		}
		return mtVideo, "wmv"
	case has(0, "OggS"):
		if bytes.Contains(b, []byte("theora")) {
			return mtVideo, "ogv"
		}
		return mtAudio, "ogg"
	case has(0, "fLaC"):
		return mtAudio, "flac"
	case has(0, "ID3"), len(b) > 1 && b[0] == 0xff && (b[1]&0xe0) == 0xe0 && (b[1]&0x06) != 0: // MP3-Frame
		return mtAudio, "mp3"
	case has(0, "{\\rtf"):
		return mtText, "rtf"
	}
	ct := strings.ToLower(ctype)
	if ct == "" || strings.HasPrefix(ct, "application/octet-stream") {
		ct = http.DetectContentType(b)
	}
	switch {
	case strings.HasPrefix(ct, "text/html"), strings.HasPrefix(ct, "application/xhtml"):
		return mtText, "html"
	case strings.HasPrefix(ct, "text/xml"), strings.HasPrefix(ct, "application/xml"):
		return mtText, "xml"
	case strings.HasPrefix(ct, "text/"), strings.HasPrefix(ct, "application/json"):
		return mtText, "txt"
	}
	return mtNone, ""
}

// sniffZip unterscheidet Office-Dokumente, EPUB und gewöhnliche ZIP-Archive am ersten Eintrag.
func sniffZip(b []byte) (int, string) {
	if len(b) < 30 {
		return mtArchive, "zip"
	}
	nl := int(b[26]) | int(b[27])<<8
	if 30+nl > len(b) {
		return mtArchive, "zip"
	}
	name := string(b[30 : 30+nl])
	rest := b[30+nl:]
	switch {
	case name == "mimetype" && bytes.Contains(rest[:min(len(rest), 120)], []byte("epub")):
		return mtArchive, "epub"
	case name == "mimetype" && bytes.Contains(rest[:min(len(rest), 120)], []byte("opendocument.text")):
		return mtOffice, "odt"
	case name == "mimetype" && bytes.Contains(rest[:min(len(rest), 120)], []byte("opendocument.presentation")):
		return mtOffice, "odp"
	case name == "mimetype" && bytes.Contains(rest[:min(len(rest), 120)], []byte("opendocument.spreadsheet")):
		return mtOffice, "ods"
	case name == "[Content_Types].xml" || strings.HasPrefix(name, "word/") || strings.HasPrefix(name, "xl/") ||
		strings.HasPrefix(name, "ppt/") || name == "_rels/.rels" || name == "docProps/app.xml":
		return mtOffice, "docx" // DOCX/XLSX/PPTX werden gleich ausgewertet
	}
	return mtArchive, "zip"
}
