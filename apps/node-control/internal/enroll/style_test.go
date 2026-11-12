package enroll

import (
	"regexp"
	"strings"
	"testing"
)

// The public pages use the console's style (fonts, colours) from one stylesheet, which the public listener serves.
func TestPublicStyleIsServed(t *testing.T) {
	f := newPubFx(t)
	css := f.get("/s/style.css")
	if css.Code != 200 || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") ||
		css.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %v", css.Code, css.Header())
	}
	body := css.Body.String()
	for _, want := range []string{`"IBM Plex Sans"`, "prefers-color-scheme:light", "--bg:#0e0f11"} {
		if !strings.Contains(body, want) {
			t.Errorf("style.css lacks %s", want)
		}
	}
	// Each font the stylesheet names is served.
	fonts := regexp.MustCompile(`url\((/s/fonts/[^)]+)\)`).FindAllStringSubmatch(body, -1)
	if len(fonts) == 0 {
		t.Fatal("style.css names no font")
	}
	for _, m := range fonts {
		r := f.get(m[1])
		if r.Code != 200 || r.Header().Get("Content-Type") != "font/woff2" || r.Body.Len() < 1000 {
			t.Errorf("%s: %d %v", m[1], r.Code, r.Header())
		}
	}
	for _, p := range []string{"/s/fonts/OFL.txt", "/s/fonts/nope.woff2", "/s/fonts/..%2fhl.css", "/s/other.css"} {
		if r := f.get(p); r.Code != 404 {
			t.Errorf("%s: %d, want 404", p, r.Code)
		}
	}
}

// The pages hold no inline style: the policy allows the own stylesheet and fonts only.
func TestPublicPagesUseTheStylesheet(t *testing.T) {
	for name, page := range map[string]string{"me": meHTML, "pick": pickHTML, "app": appHTML, "landing": landingTemplate} {
		if !strings.Contains(page, `<link rel="stylesheet" href="/s/style.css">`) || strings.Contains(page, "<style") ||
			strings.Contains(page, "style=") {
			t.Errorf("%s: must link /s/style.css and hold no inline style", name)
		}
	}
	f := newPubFx(t)
	d, _ := f.newDevice("Anna", "anna.owner", "karing")
	rec := f.get("/e/" + f.token(d))
	if !strings.Contains(rec.Body.String(), `href="/s/style.css"`) {
		t.Fatalf("landing page: %s", rec.Body.String())
	}
}
