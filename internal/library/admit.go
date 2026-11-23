// Package library is the account's central store: the files and notes agents
// and people keep for an account and for each app it runs -- a brand guide, a
// logo, theme instructions, what was decided last time.
//
// Everything in it ends up inside workspace machines, read by agents and by
// whatever tools they reach for. That is the threat this file exists for. A
// file is admitted only if it is one of a handful of inert kinds, judged by
// its bytes rather than its name or what the uploader claimed, and the kinds
// that can carry active content are made safe or refused:
//
//   - Images are decoded and encoded again. What is stored is pixels this
//     process wrote, so a polyglot, an oversized header aimed at a decoder, or
//     a script tucked in metadata does not survive.
//   - PDFs cannot be rewritten without a PDF library, so they are scanned --
//     including inside their compressed streams -- and refused if they carry
//     anything that acts: JavaScript, launch and submit actions, embedded
//     files, rich media, XFA, or encryption that would hide any of those.
//   - Text must be valid UTF-8 with no control characters, and may not start
//     with "#!": nothing in the library is a program.
//
// Everything else is refused, SVG and HTML included, because both are
// documents that run code when something renders them.
package library

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxFileBytes is the most one file may hold.
const MaxFileBytes = 10 << 20

// Image limits, checked from the header before any pixels are decoded, so a
// small file declaring a huge canvas cannot make this process allocate it.
const (
	maxImageSide   = 8000
	maxImagePixels = 25_000_000
)

// Kind is what sort of thing a file is.
type Kind string

const (
	KindImage Kind = "image"
	KindPDF   Kind = "pdf"
	KindText  Kind = "text"
)

// ErrRefused wraps every reason a file is not admitted.
var ErrRefused = errors.New("library: file refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Admitted is a file that may be stored: its cleaned path, what it is, and
// the bytes to keep, which for an image are not the bytes that were sent.
type Admitted struct {
	Path        string
	Kind        Kind
	ContentType string
	Data        []byte
}

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,79}$`)

// CleanPath checks a library path: up to six folders deep, each part made of
// ordinary characters, nothing hidden, nothing that climbs out.
func CleanPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.Trim(p, "/")
	if p == "" || len(p) > 200 {
		return "", refuse("a path needs 1 to 200 characters")
	}
	parts := strings.Split(p, "/")
	if len(parts) > 6 {
		return "", refuse("%q is nested too deeply", p)
	}
	for _, part := range parts {
		if !segmentPattern.MatchString(part) || strings.Contains(part, "..") {
			return "", refuse("%q is not an allowed name: use letters, digits, spaces, dots, dashes and underscores, and do not start with a dot", part)
		}
	}
	return strings.Join(parts, "/"), nil
}

var textExtensions = map[string]string{
	".txt":      "text/plain; charset=utf-8",
	".md":       "text/markdown; charset=utf-8",
	".markdown": "text/markdown; charset=utf-8",
	".csv":      "text/csv; charset=utf-8",
	".json":     "application/json",
}

// Admit decides whether a file may enter the library, and returns what to
// store if it may.
func Admit(name string, data []byte) (*Admitted, error) {
	clean, err := CleanPath(name)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, refuse("%s is empty", clean)
	}
	if len(data) > MaxFileBytes {
		return nil, refuse("%s is larger than %d MiB", clean, MaxFileBytes>>20)
	}
	ext := strings.ToLower(path.Ext(clean))

	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return admitImage(clean, ext, data, "png")
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return admitImage(clean, ext, data, "jpeg")
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return admitImage(clean, ext, data, "gif")
	case bytes.HasPrefix(data, []byte("RIFF")) && len(data) > 12 && string(data[8:12]) == "WEBP":
		return nil, refuse("%s is WebP, which is not accepted yet: save it as PNG or JPEG", clean)
	case bytes.HasPrefix(data, []byte("%PDF-")):
		if ext != ".pdf" {
			return nil, refuse("%s is a PDF, so its name must end in .pdf", clean)
		}
		if err := inspectPDF(data); err != nil {
			return nil, refuse("%s: %v", clean, err)
		}
		return &Admitted{Path: clean, Kind: KindPDF, ContentType: "application/pdf", Data: data}, nil
	}

	contentType, ok := textExtensions[ext]
	if !ok {
		return nil, refuse("%s is not a kind of file the library keeps: PNG, JPEG, GIF, PDF, or text (.txt, .md, .csv, .json)", clean)
	}
	if err := inspectText(data); err != nil {
		return nil, refuse("%s: %v", clean, err)
	}
	if ext == ".json" && !json.Valid(data) {
		return nil, refuse("%s is not valid JSON", clean)
	}
	return &Admitted{Path: clean, Kind: KindText, ContentType: contentType, Data: data}, nil
}

var imageExtensions = map[string][]string{
	"png":  {".png"},
	"jpeg": {".jpg", ".jpeg"},
	"gif":  {".gif"},
}

func admitImage(clean, ext string, data []byte, format string) (*Admitted, error) {
	allowed := false
	for _, e := range imageExtensions[format] {
		allowed = allowed || ext == e
	}
	if !allowed {
		return nil, refuse("%s is a %s image, so its name must end in %s",
			clean, strings.ToUpper(format), strings.Join(imageExtensions[format], " or "))
	}

	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, refuse("%s is not a readable image: %v", clean, err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxImageSide ||
		config.Height > maxImageSide || config.Width*config.Height > maxImagePixels {
		return nil, refuse("%s is %dx%d; images may be at most %d pixels on a side",
			clean, config.Width, config.Height, maxImageSide)
	}

	var out bytes.Buffer
	contentType := "image/" + format
	switch format {
	case "png":
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, refuse("%s is not a readable PNG: %v", clean, err)
		}
		err = png.Encode(&out, img)
		if err != nil {
			return nil, err
		}
	case "jpeg":
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, refuse("%s is not a readable JPEG: %v", clean, err)
		}
		if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 92}); err != nil {
			return nil, err
		}
	case "gif":
		animation, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return nil, refuse("%s is not a readable GIF: %v", clean, err)
		}
		if len(animation.Image) > 300 {
			return nil, refuse("%s has %d frames; at most 300", clean, len(animation.Image))
		}
		// Only the frames and their timing are carried over: comments and
		// application extensions are exactly where things get hidden.
		kept := &gif.GIF{Image: animation.Image, Delay: animation.Delay,
			Disposal: animation.Disposal, LoopCount: animation.LoopCount,
			Config: animation.Config, BackgroundIndex: animation.BackgroundIndex}
		if err := gif.EncodeAll(&out, kept); err != nil {
			return nil, err
		}
	}
	if out.Len() > MaxFileBytes {
		return nil, refuse("%s is larger than %d MiB once re-encoded", clean, MaxFileBytes>>20)
	}
	return &Admitted{Path: clean, Kind: KindImage, ContentType: contentType, Data: out.Bytes()}, nil
}

func inspectText(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("text must be UTF-8")
	}
	trimmed := bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if bytes.HasPrefix(trimmed, []byte("#!")) {
		return errors.New("starts with #!, which makes it a script; the library keeps no programs")
	}
	for _, r := range string(trimmed) {
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return fmt.Errorf("contains control character U+%04X", r)
		}
		if r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return fmt.Errorf("contains control character U+%04X", r)
		}
	}
	return nil
}

// activePDFNames are the PDF keys that make a document do something rather
// than show something. /URI is not among them: a link is a link.
var activePDFNames = []string{
	"/JavaScript", "/JS", "/Launch", "/EmbeddedFile", "/EmbeddedFiles",
	"/RichMedia", "/XFA", "/AA", "/OpenAction", "/SubmitForm", "/ImportData",
	"/GoToE", "/GoToR", "/Sound", "/Movie", "/Encrypt",
}

var (
	pdfNameEscape = regexp.MustCompile(`#([0-9A-Fa-f]{2})`)
	pdfStream     = regexp.MustCompile(`(?s)stream\r?\n`)
	pdfNameAfter  = regexp.MustCompile(`^[^\s/<>\[\]()%{}]`)
)

// inspectPDF refuses a PDF that carries anything active, looking inside every
// compressed stream too, since an object stream is where a hidden action
// would be.
func inspectPDF(data []byte) error {
	if !bytes.Contains(data[max(0, len(data)-1024):], []byte("%%EOF")) {
		return errors.New("not a complete PDF")
	}
	if err := scanPDFText(data); err != nil {
		return err
	}
	// Every stream that inflates is scanned as well. One that declares
	// itself an object stream and cannot be read is refused, because that is
	// exactly what would hide an action from the scan above.
	rest := data
	for {
		loc := pdfStream.FindIndex(rest)
		if loc == nil {
			return nil
		}
		header := rest[max(0, loc[0]-512):loc[0]]
		body := rest[loc[1]:]
		end := bytes.Index(body, []byte("endstream"))
		if end < 0 {
			return errors.New("a stream never ends")
		}
		inflated, err := inflate(body[:end])
		switch {
		case err == nil:
			if err := scanPDFText(inflated); err != nil {
				return err
			}
		case bytes.Contains(header, []byte("/ObjStm")):
			return errors.New("contains an object stream that cannot be read, so it cannot be checked")
		}
		rest = body[end:]
	}
}

func inflate(raw []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// Bounded, so a small stream that inflates without end is a refusal and
	// not this process running out of memory.
	out, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return out, nil
}

func scanPDFText(data []byte) error {
	// Names may spell letters as #xx; undo that before looking, or /J#61vaScript
	// would pass as something else.
	normal := pdfNameEscape.ReplaceAllFunc(data, func(m []byte) []byte {
		var b byte
		fmt.Sscanf(string(m[1:]), "%02x", &b)
		return []byte{b}
	})
	for _, name := range activePDFNames {
		for i := 0; ; {
			at := bytes.Index(normal[i:], []byte(name))
			if at < 0 {
				break
			}
			after := normal[i+at+len(name):]
			// A whole name only: /JSON is not /JS.
			if len(after) == 0 || !pdfNameAfter.Match(after[:1]) {
				return fmt.Errorf("contains %s, which makes a PDF do something rather than show something", name)
			}
			i += at + len(name)
		}
	}
	return nil
}
