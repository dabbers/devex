package proxy

import (
	"strings"
	"testing"
)

func TestRouteValidate(t *testing.T) {
	valid := Route{Hostname: "preview-web-ratings.dab.im", UpstreamHost: "172.30.0.2", UpstreamPort: 5173}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(valid) = %v", err)
	}
	if got := valid.Upstream(); got != "172.30.0.2:5173" {
		t.Fatalf("Upstream() = %q", got)
	}

	for name, route := range map[string]Route{
		"no hostname":    {UpstreamHost: "h", UpstreamPort: 1},
		"no upstream":    {Hostname: "h.dab.im", UpstreamPort: 1},
		"port zero":      {Hostname: "h.dab.im", UpstreamHost: "h"},
		"port too large": {Hostname: "h.dab.im", UpstreamHost: "h", UpstreamPort: 70000},
		"space in host":  {Hostname: "bad host.dab.im", UpstreamHost: "h", UpstreamPort: 1},
		"slash in host":  {Hostname: "h.dab.im/x", UpstreamHost: "h", UpstreamPort: 1},
	} {
		if err := route.Validate(); err == nil {
			t.Errorf("Validate(%s) = nil, want an error", name)
		}
	}
}

func TestNormalizeRejectsDuplicateHostnames(t *testing.T) {
	// Two forks answering on one hostname would make the preview a coin flip.
	_, err := Normalize([]Route{
		{Hostname: "preview-a.dab.im", UpstreamHost: "10.0.0.1", UpstreamPort: 3000},
		{Hostname: "PREVIEW-A.dab.im", UpstreamHost: "10.0.0.2", UpstreamPort: 3000},
	})
	if err == nil {
		t.Fatal("expected duplicate hostnames to be rejected, case-insensitively")
	}
	if !strings.Contains(err.Error(), "preview-a.dab.im") {
		t.Fatalf("error should name the conflicting hostname: %v", err)
	}
}

func TestNormalizeSortsAndLowercases(t *testing.T) {
	got, err := Normalize([]Route{
		{Hostname: "preview-z.dab.im", UpstreamHost: "10.0.0.3", UpstreamPort: 3000},
		{Hostname: "Preview-A.dab.im", UpstreamHost: "10.0.0.1", UpstreamPort: 3000},
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got[0].Hostname != "preview-a.dab.im" || got[1].Hostname != "preview-z.dab.im" {
		t.Fatalf("Normalize did not sort and lowercase: %+v", got)
	}
}

func TestNormalizePropagatesValidationErrors(t *testing.T) {
	if _, err := Normalize([]Route{{Hostname: "ok.dab.im"}}); err == nil {
		t.Fatal("expected an invalid route to fail normalization")
	}
}

func TestNormalizeEmptyIsNotAnError(t *testing.T) {
	got, err := Normalize(nil)
	if err != nil {
		t.Fatalf("Normalize(nil) = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Normalize(nil) returned %d routes", len(got))
	}
}
