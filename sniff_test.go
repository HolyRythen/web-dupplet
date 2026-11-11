package main

import (
	"archive/zip"
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestSniff(t *testing.T) {
	var pngB bytes.Buffer
	png.Encode(&pngB, image.NewGray(image.Rect(0, 0, 4, 4)))
	docx := zipMit(t, map[string]string{"[Content_Types].xml": "<Types/>"})
	var epub bytes.Buffer
	zw := zip.NewWriter(&epub)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	w.Write([]byte("application/epub+zip"))
	zw.Close()
	tarB := make([]byte, 512)
	copy(tarB[257:], "ustar")
	cases := []struct {
		name string
		b    []byte
		ct   string
		mt   int
		ext  string
	}{
		{"JPEG", []byte("\xff\xd8\xff\xe0\x00\x10JFIF"), "", mtImage, "jpg"},
		{"PNG", pngB.Bytes(), "", mtImage, "png"},
		{"PDF", []byte("%PDF-1.7\n%"), "", mtPDF, "pdf"},
		{"DOCX", docx, "", mtOffice, "docx"},
		{"EPUB", epub.Bytes(), "", mtArchive, "epub"},
		{"ZIP", zipMit(t, map[string]string{"a.txt": "x"}), "", mtArchive, "zip"},
		{"TAR", tarB, "", mtArchive, "tar"},
		{"GZ", []byte("\x1f\x8b\x08\x00"), "", mtArchive, "tgz"},
		{"MP4", []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00"), "", mtVideo, "mp4"},
		{"M4A", []byte("\x00\x00\x00\x20ftypM4A \x00\x00\x02\x00"), "", mtAudio, "m4a"},
		{"MKV", []byte("\x1a\x45\xdf\xa3\x9f\x42\x86\x81"), "", mtVideo, "mkv"},
		{"MP3", []byte("ID3\x04\x00\x00\x00\x00\x00\x00"), "", mtAudio, "mp3"},
		{"OGG", []byte("OggS\x00\x02....\x01vorbis"), "", mtAudio, "ogg"},
		{"OGV", []byte("OggS\x00\x02....\x80theora"), "", mtVideo, "ogv"},
		{"HTML", []byte("<!DOCTYPE html><html><body>Hallo</body></html>"), "", mtText, "html"},
		{"Text per Content-Type", []byte("einfach nur Text"), "text/plain; charset=utf-8", mtText, "txt"},
		{"Binärdatei", []byte{0, 1, 2, 3, 0xfe, 0x00, 0x13}, "", mtNone, ""},
	}
	for _, c := range cases {
		mt, ext := sniff(c.b, c.ct)
		if mt != c.mt || ext != c.ext {
			t.Errorf("%s: (%d,%q), erwartet (%d,%q)", c.name, mt, ext, c.mt, c.ext)
		}
	}
}
