package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// What the owner chose for the settings this app declares in
// config/settings.json. The platform writes the chosen values to a file and
// rewrites it whenever they change; this reads it again when it has, so a new
// colour shows on the next page load with no restart.
//
// The file is the platform's, but it is still input: every value is checked
// again here before it reaches a page, and anything unexpected falls back to
// the default rather than being drawn.

const settingsEnv = "DABBERZ_SETTINGS_FILE"

// site is what the theme draws from. Its zero value is never used: see
// defaultSite.
type site struct {
	Name     string
	Tagline  string
	Accent   string
	Header   string
	LogoURL  string
	Thanks   string
	ShowLogo bool
}

var defaultSite = site{
	Name:    "Forms",
	Tagline: "on dabberz",
	Accent:  "#7a1f2b",
	Header:  "#3a1015",
}

var (
	hexColour = regexp.MustCompile(`^#[0-9a-f]{6}$`)
	assetURL  = regexp.MustCompile(`^/_dabberz/asset/[0-9a-f]{64}\.(png|jpg|gif)$`)
)

type settingsFile struct {
	Values map[string]any `json:"values"`
	Images map[string]struct {
		URL string `json:"url"`
	} `json:"images"`
}

type siteSettings struct {
	path string

	mu      sync.Mutex
	checked time.Time
	modTime time.Time
	size    int64
	current site
}

func newSiteSettings() *siteSettings {
	path := os.Getenv(settingsEnv)
	if path == "" {
		path = ".dabberz/settings.json"
	}
	return &siteSettings{path: path, current: defaultSite}
}

// get returns the current settings, looking at the file at most once a second.
func (s *siteSettings) get() site {
	if s == nil {
		return defaultSite
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.Sub(s.checked) < time.Second {
		return s.current
	}
	s.checked = now
	info, err := os.Stat(s.path)
	if err != nil {
		s.current, s.modTime, s.size = defaultSite, time.Time{}, 0
		return s.current
	}
	if info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return s.current
	}
	s.modTime, s.size = info.ModTime(), info.Size()
	s.current = readSite(s.path)
	return s.current
}

func readSite(path string) site {
	out := defaultSite
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 1<<20 {
		return out
	}
	var f settingsFile
	if json.Unmarshal(raw, &f) != nil {
		return out
	}
	text := func(key string, limit int, into *string) {
		if v, ok := f.Values[key].(string); ok {
			v = strings.TrimSpace(v)
			if v != "" && len([]rune(v)) <= limit {
				*into = v
			}
		}
	}
	colour := func(key string, into *string) {
		if v, ok := f.Values[key].(string); ok && hexColour.MatchString(v) {
			*into = v
		}
	}
	text("/brand/name", 80, &out.Name)
	text("/brand/tagline", 120, &out.Tagline)
	colour("/brand/accent", &out.Accent)
	colour("/brand/header", &out.Header)
	text("/answers/thanks", 600, &out.Thanks)
	if img, ok := f.Images["/brand/logo"]; ok && assetURL.MatchString(img.URL) {
		out.LogoURL, out.ShowLogo = img.URL, true
	}
	return out
}
