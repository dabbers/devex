package appsettings

import (
	"os"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) *Spec {
	t.Helper()
	s, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

func refusedSpec(t *testing.T, why, raw string) {
	t.Helper()
	if _, err := Parse([]byte(raw)); err == nil {
		t.Errorf("accepted %s", why)
	}
}

// TestFormsDeclaresItsSettings: the catalog app's own declaration is one the
// platform accepts, so a fresh install has a settings panel.
func TestFormsDeclaresItsSettings(t *testing.T) {
	raw, err := os.ReadFile("../../apps/forms/config/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	s := mustParse(t, string(raw))
	if s.Setting("/brand/accent") == nil || s.Setting("/brand/logo") == nil {
		t.Fatal("Forms does not declare its accent colour and logo")
	}
}

const one = `{"version":1,"root":["a"],"components":[%s]}`

func spec(components string) string { return strings.Replace(one, "%s", components, 1) }

// TestADeclarationIsDataNotMarkup: nothing outside the catalog, no field the
// panel does not know, no control characters -- so there is nothing an agent
// can write here that the panel would run or render as markup.
func TestADeclarationIsDataNotMarkup(t *testing.T) {
	refusedSpec(t, "a component outside the catalog", spec(`{"id":"a","type":"html","label":"x","path":"/a"}`))
	refusedSpec(t, "a script component", spec(`{"id":"a","type":"script","text":"alert(1)"}`))
	refusedSpec(t, "a field the panel does not know", spec(`{"id":"a","type":"text","label":"x","path":"/a","html":"<b>x</b>"}`))
	refusedSpec(t, "an onclick", spec(`{"id":"a","type":"text","label":"x","path":"/a","onclick":"alert(1)"}`))
	refusedSpec(t, "a style", spec(`{"id":"a","type":"note","text":"x","style":"position:fixed"}`))
	refusedSpec(t, "a control character", spec(`{"id":"a","type":"text","label":"x\u001b[31m","path":"/a"}`))
	refusedSpec(t, "a second JSON value", spec(`{"id":"a","type":"note","text":"x"}`)+`{}`)
	refusedSpec(t, "an unknown version", strings.Replace(spec(`{"id":"a","type":"note","text":"x"}`), `"version":1`, `"version":2`, 1))
	refusedSpec(t, "a default that does not fit", spec(`{"id":"a","type":"color","label":"x","path":"/a","default":"red"}`))
	refusedSpec(t, "a default picture", spec(`{"id":"a","type":"image","label":"x","path":"/a","default":"lib_00000000000000000000000000"}`))
	refusedSpec(t, "a path that is not one", spec(`{"id":"a","type":"text","label":"x","path":"../a"}`))
}

func TestADeclarationIsATree(t *testing.T) {
	refusedSpec(t, "a cycle", `{"version":1,"root":["a"],"components":[
		{"id":"a","type":"section","title":"A","children":["b"]},
		{"id":"b","type":"section","title":"B","children":["a"]}]}`)
	refusedSpec(t, "a component used twice", `{"version":1,"root":["a","b"],"components":[
		{"id":"a","type":"section","title":"A","children":["b"]},
		{"id":"b","type":"note","text":"x"}]}`)
	refusedSpec(t, "an orphan", `{"version":1,"root":["a"],"components":[
		{"id":"a","type":"note","text":"x"},{"id":"b","type":"note","text":"y"}]}`)
	refusedSpec(t, "a missing child", `{"version":1,"root":["a"],"components":[
		{"id":"a","type":"section","title":"A","children":["nope"]}]}`)
	refusedSpec(t, "two settings at one path", `{"version":1,"root":["a","b"],"components":[
		{"id":"a","type":"text","label":"A","path":"/x"},{"id":"b","type":"text","label":"B","path":"/x"}]}`)
}

// TestValuesFitTheirSettings: what an owner submits is checked against what
// the app declares, and what an app is told is always declared and valid.
func TestValuesFitTheirSettings(t *testing.T) {
	s := mustParse(t, `{"version":1,"root":["c","n","p","l","t"],"components":[
		{"id":"c","type":"color","label":"Colour","path":"/c","default":"#112233"},
		{"id":"n","type":"number","label":"Count","path":"/n","min":1,"max":5},
		{"id":"p","type":"select","label":"Plan","path":"/p","options":[{"value":"a","label":"A"}]},
		{"id":"l","type":"image","label":"Logo","path":"/l"},
		{"id":"t","type":"text","label":"Name","path":"/t","maxLength":5}]}`)
	logo := "lib_0123456789abcdefghjkmnpqrs"
	ok := func(id string) bool { return id == logo }

	for _, bad := range []map[string]any{
		{"/c": "red"}, {"/c": "#11223"}, {"/n": 9.0}, {"/n": "3"}, {"/p": "b"},
		{"/t": "toolong"}, {"/t": "a\nb"}, {"/l": "lib_somebody_elses_file_000"},
		{"/l": "lib_00000000000000000000000000"}, {"/unknown": "x"},
	} {
		if _, err := s.Check(bad, ok); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	got, err := s.Check(map[string]any{"/c": "#AABBCC", "/n": 3.0, "/l": logo}, ok)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got["/c"] != "#aabbcc" {
		t.Errorf("colour kept as %v", got["/c"])
	}

	effective := s.Effective(map[string]any{"/n": 4.0, "/gone": "kept for later", "/c": "not a colour"})
	if effective["/c"] != "#112233" || effective["/n"] != 4.0 {
		t.Errorf("effective = %v", effective)
	}
	if _, ok := effective["/gone"]; ok {
		t.Error("a value for a setting the app no longer has was passed to it")
	}
}
