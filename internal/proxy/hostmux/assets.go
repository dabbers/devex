package hostmux

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// assetPrefix is where the edge serves the pictures an app's settings point
// at -- the logo an owner chose -- so an app shows one by linking to it and
// needs no code of its own for storing or serving it.
const assetPrefix = reservedPrefix + "asset/"

// ErrNoAsset means there is no such picture for this hostname.
var ErrNoAsset = errors.New("hostmux: no such asset")

// Assets finds a picture an app's settings use. Only pictures a setting of
// the app behind host currently points at are found: the library holds far
// more than any one site may show, and most of it is not public.
type Assets interface {
	Asset(ctx context.Context, host, name string) (contentType string, data []byte, err error)
}

// WithAssets serves the pictures an app's settings use.
func (m *Mux) WithAssets(a Assets) *Mux {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.assets = a
	return m
}

var assetName = regexp.MustCompile(`^[0-9a-f]{64}\.(png|jpg|gif)$`)

var assetTypes = map[string]string{"png": "image/png", "jpg": "image/jpeg", "gif": "image/gif"}

// serveAsset answers /_dabberz/asset/<sha256>.<ext>. A picture is as visible
// as the site's front page: open on an open site, for members on a locked one
// or a change's preview.
func (m *Mux) serveAsset(w http.ResponseWriter, r *http.Request, host string, rt *route) {
	m.mu.RLock()
	assets := m.assets
	m.mu.RUnlock()
	name := strings.TrimPrefix(r.URL.Path, assetPrefix)
	if assets == nil || !assetName.MatchString(name) ||
		(r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	r = m.withVisitor(r)
	shut := rt.membersOnly(&url.URL{Path: "/"})
	if shut {
		who, known := visitor(r.Context())
		if !rt.isMember(who, known) {
			m.refuse(w, r, host, who, known)
			return
		}
	}
	contentType, data, err := assets.Asset(r.Context(), host, name)
	if errors.Is(err, ErrNoAsset) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		m.logger.Error("could not serve an asset", "host", host, "asset", name, "error", err)
		http.Error(w, "this picture is unavailable", http.StatusBadGateway)
		return
	}
	ext := name[strings.LastIndexByte(name, '.')+1:]
	if contentType != assetTypes[ext] {
		// The name says what it is; the bytes have to agree.
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	// Opened on its own, it is a picture and nothing else.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-site")
	if shut {
		h.Set("Cache-Control", "private, no-store")
	} else {
		// Named by its hash, so it never changes under the same name.
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
