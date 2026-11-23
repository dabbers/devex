package hostmux

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/domain"
)

type stubAssets struct{ asked []string }

func (s *stubAssets) Asset(_ context.Context, host, name string) (string, []byte, error) {
	s.asked = append(s.asked, host+" "+name)
	if strings.HasPrefix(name, strings.Repeat("a", 64)) {
		return "image/png", []byte("\x89PNG pixels"), nil
	}
	if strings.HasPrefix(name, strings.Repeat("b", 64)) {
		return "text/html", []byte("<script>alert(1)</script>"), nil
	}
	return "", nil, ErrNoAsset
}

// TestALogoIsServedByTheEdgeAsAPictureOnly: an app links to its owner's logo
// and the edge serves it -- as an inert image, only where the site's front
// page is visible, and never as anything else.
func TestALogoIsServedByTheEdgeAsAPictureOnly(t *testing.T) {
	t.Parallel()
	logo := assetPrefix + strings.Repeat("a", 64) + ".png"

	mux, app := edgeFor(t, open, nil)
	assets := &stubAssets{}
	mux.WithAssets(assets)
	rec := visit(mux, "GET", logo)
	if rec.Code != http.StatusOK || rec.Body.String() != "\x89PNG pixels" {
		t.Fatalf("an open site's logo: %d %q", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("served without the headers that keep it a picture: %v", h)
	}
	if len(app.requests()) != 0 {
		t.Fatal("the app was asked for the edge's own path")
	}
	if len(assets.asked) != 1 || !strings.HasPrefix(assets.asked[0], appHost+" ") {
		t.Fatalf("the asset was not looked up for this hostname: %v", assets.asked)
	}

	for _, bad := range []string{
		assetPrefix + strings.Repeat("c", 64) + ".png",         // not one of this site's
		assetPrefix + strings.Repeat("b", 64) + ".png",         // bytes that are not what the name says
		assetPrefix + strings.Repeat("a", 64) + ".svg",         // not a kind the edge serves
		assetPrefix + "../" + strings.Repeat("a", 61) + ".png", // not a name
		assetPrefix + strings.Repeat("A", 64) + ".png",
	} {
		if rec := visit(mux, "GET", bad); rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d", bad, rec.Code)
		}
	}
	if rec := httptest.NewRecorder(); true {
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "http://"+appHost+logo, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("a POST for a logo answered %d", rec.Code)
		}
	}

	// A locked site's logo is for its members, like the rest of it.
	locked, _ := edgeFor(t, domain.Access{Mode: domain.AccessInternal}, nil)
	locked.WithAssets(&stubAssets{})
	req := httptest.NewRequest("GET", "http://"+appHost+logo, nil)
	shut := httptest.NewRecorder()
	locked.ServeHTTP(shut, req)
	if shut.Code == http.StatusOK {
		t.Fatal("a locked site's logo was served to a stranger")
	}
	member, _ := edgeFor(t, domain.Access{Mode: domain.AccessInternal}, owner)
	member.WithAssets(&stubAssets{})
	if rec := visit(member, "GET", logo); rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("a member of a locked site: %d, cache %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}
