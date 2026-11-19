package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dabbers/devex/internal/previewpass"
)

// TestAPassShowsThePreviewToItsOwnerAndNobodyElse is the browser half of
// checking as the owner, in a real Chromium.
//
// The preview answers 401 without the pass, as Forms' front page does. It
// loads an image from a second host -- localhost rather than 127.0.0.1,
// because cookies are scoped by host and not by port -- standing in for
// anything a page pulls in from elsewhere, which must never be sent the pass.
// And the page throws if its own scripts can read the pass, which the harness
// reports as a failure: the page is code an agent wrote.
func TestAPassShowsThePreviewToItsOwnerAndNobodyElse(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a browser")
	}
	requireBrowser(t)
	const pass = "the-owners-pass"

	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), previewpass.CookieName) {
			leaked.Add(1)
		}
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"))
	}))
	defer elsewhere.Close()
	third, err := url.Parse(elsewhere.URL)
	if err != nil {
		t.Fatal(err)
	}

	preview := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(previewpass.CookieName); err != nil || c.Value != pass {
			http.Error(w, "Sign in", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><title>Your forms</title>
<h1>Your forms</h1><p>Nothing here yet.</p>
<img src="http://localhost:%s/pixel.gif" alt="">
<script>if (document.cookie.indexOf(%q) >= 0) { throw new Error("the page can read the pass"); }</script>`,
			third.Port(), previewpass.CookieName)
	}))
	defer preview.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stranger := run(ctx, job{ForkID: "anon", PreviewURL: preview.URL + "/"}, settings{settle: time.Second})
	if stranger.Passed {
		t.Fatal("a page behind sign-in passed for a visitor with no pass")
	}

	owner := run(ctx, job{ForkID: "owner", PreviewURL: preview.URL + "/", Pass: pass}, settings{settle: time.Second})
	if !owner.Passed {
		t.Fatalf("the owner's view of the preview did not pass: %+v", owner.Checks)
	}
	if n := leaked.Load(); n != 0 {
		t.Fatalf("the pass was sent to another host %d time(s)", n)
	}
}
