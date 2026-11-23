package library

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func refused(t *testing.T, name string, data []byte, why string) {
	t.Helper()
	_, err := Admit(name, data)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("%s (%s) was admitted: %v", name, why, err)
	}
}

// TestAPictureIsRewrittenNotKept: what is stored is pixels this process wrote,
// so anything riding along in the original -- here a script appended after
// the image, which a careless reader might find -- is gone.
func TestAPictureIsRewrittenNotKept(t *testing.T) {
	original := append(pngBytes(t), []byte("<script>alert(1)</script>#!/bin/sh\nrm -rf /")...)
	got, err := Admit("brand/logo.png", original)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if got.Kind != KindImage || got.ContentType != "image/png" {
		t.Fatalf("kind %s type %s", got.Kind, got.ContentType)
	}
	if bytes.Contains(got.Data, []byte("script")) || bytes.Contains(got.Data, []byte("rm -rf")) {
		t.Fatal("what was appended to the picture survived")
	}
	if _, err := png.Decode(bytes.NewReader(got.Data)); err != nil {
		t.Fatalf("what was kept is not a picture: %v", err)
	}
}

// TestAHugeCanvasIsRefusedBeforeItIsDecoded: a few bytes can claim a canvas
// of billions of pixels, and decoding it would be this process allocating it.
func TestAHugeCanvasIsRefusedBeforeItIsDecoded(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 100000)
	binary.BigEndian.PutUint32(ihdr[4:], 100000)
	ihdr[8], ihdr[9] = 8, 6
	chunk := func(kind string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.WriteString(kind)
		b.Write(data)
		binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(kind), data...)))
	}
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	refused(t, "huge.png", b.Bytes(), "a 100000x100000 canvas")
}

func TestOnlyInertKindsAreKept(t *testing.T) {
	refused(t, "logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), "SVG runs script")
	refused(t, "page.html", []byte("<html><script>alert(1)</script></html>"), "HTML")
	refused(t, "tool", []byte("\x7fELF\x02\x01\x01\x00rest"), "an executable")
	refused(t, "tool.txt", []byte("\x7fELF\x02\x01\x01\x00rest"), "an executable named as text")
	refused(t, "run.txt", []byte("#!/bin/sh\necho hi\n"), "a script named as text")
	refused(t, "run.md", []byte("\xef\xbb\xbf#!/usr/bin/env python\n"), "a script behind a byte order mark")
	refused(t, "archive.txt", []byte("PK\x03\x04\x14\x00\x00\x00"), "a zip named as text")
	refused(t, "notes.txt", []byte("hello\x00world"), "text with a NUL")
	refused(t, "notes.txt", []byte("\xff\xfe\x00h"), "text that is not UTF-8")
	refused(t, "logo.webp", append([]byte("RIFF\x00\x00\x00\x00WEBP"), make([]byte, 20)...), "WebP, not yet")
	refused(t, "logo.jpg", pngBytes(t), "a PNG named as a JPEG")
	refused(t, "data.json", []byte("{not json"), "JSON that is not")
	refused(t, "notes.txt", nil, "nothing at all")
}

func TestPathsStayInsideTheLibrary(t *testing.T) {
	ok := []byte("fine")
	for _, p := range []string{"../escape.txt", "a/../../b.txt", ".hidden.txt", "a/.git/config.txt",
		"/", "", strings.Repeat("a/", 7) + "x.txt", "bad\x00name.txt", "a/b\\..\\..\\c.txt"} {
		refused(t, p, ok, "the path "+p)
	}
	got, err := Admit(" /brand\\Guide v2.md/ ", []byte("# Brand\n"))
	if err != nil || got.Path != "brand/Guide v2.md" {
		t.Fatalf("an ordinary path was not cleaned: %v %+v", err, got)
	}
}

func pdfWith(object string) []byte {
	return []byte("%PDF-1.7\n1 0 obj\n" + object + "\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")
}

func flateStream(dict, body string) string {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	w.Write([]byte(body))
	w.Close()
	return dict + "\nstream\n" + b.String() + "\nendstream"
}

func TestAPDFThatDoesSomethingIsRefused(t *testing.T) {
	if _, err := Admit("guide.pdf", pdfWith("<< /Type /Catalog /Metadata << /Subtype /JSON >> >>")); err != nil {
		t.Fatalf("a plain PDF was refused: %v", err)
	}
	refused(t, "guide.pdf", pdfWith("<< /OpenAction << /S /JavaScript /JS (app.alert(1)) >> >>"), "JavaScript")
	refused(t, "guide.pdf", pdfWith("<< /OpenAction << /S /J#61vaScript >> >>"), "JavaScript spelt in hex")
	refused(t, "guide.pdf", pdfWith("<< /Launch << /F (cmd.exe) >> >>"), "a launch action")
	refused(t, "guide.pdf", pdfWith("<< /Names << /EmbeddedFiles 2 0 R >> >>"), "an embedded file")
	refused(t, "guide.pdf", pdfWith("<< /Encrypt 3 0 R >>"), "encryption hiding the rest")
	refused(t, "guide.pdf", pdfWith(flateStream("<< /Type /ObjStm /Filter /FlateDecode >>",
		"<< /S /JavaScript /JS (app.alert(1)) >>")), "JavaScript inside a compressed object stream")
	refused(t, "guide.pdf", pdfWith("<< /Type /ObjStm /Filter /LZWDecode >>\nstream\nnot zlib at all\nendstream"),
		"an object stream that cannot be read")
	refused(t, "guide.txt", pdfWith("<< >>"), "a PDF named as text")
	refused(t, "guide.pdf", []byte("%PDF-1.7\n1 0 obj << >> endobj\n"), "a truncated PDF")
}
