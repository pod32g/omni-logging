package web

import (
	"io"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func readAsset(t *testing.T, name string) string {
	t.Helper()
	f, err := FS().Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// bundle returns the built JS and CSS. Their names carry a content hash, so the
// tests below find them rather than hard-coding a filename that changes on
// every UI edit.
func bundle(t *testing.T) (js, css string) {
	t.Helper()
	entries, err := fs.ReadDir(FS(), "assets")
	if err != nil {
		t.Fatalf("the UI bundle is missing — run `make ui`: %v", err)
	}
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".js") && js == "":
			js = readAsset(t, "assets/"+e.Name())
		case strings.HasSuffix(e.Name(), ".css") && css == "":
			css = readAsset(t, "assets/"+e.Name())
		}
	}
	if js == "" || css == "" {
		t.Fatal("the embedded bundle has no JS or no CSS — run `make ui`")
	}
	return js, css
}

// TestEmbeddedUIIsBuilt guards the mistake this layout makes easy: editing
// internal/web/ui/ and shipping the previous bundle. The built output is
// committed so `go build` needs no Node, which means a stale commit is
// invisible unless something checks.
func TestEmbeddedUIIsBuilt(t *testing.T) {
	html := readAsset(t, "index.html")
	if !strings.Contains(html, `<div id="root">`) {
		t.Error("index.html is not the built React shell")
	}
	if strings.Contains(html, "/src/main.tsx") {
		t.Error("index.html still points at the dev entry point; run `make ui`")
	}
	if !regexp.MustCompile(`assets/index-[A-Za-z0-9_-]+\.js`).MatchString(html) {
		t.Error("index.html does not reference a hashed JS bundle")
	}
	js, _ := bundle(t)
	if len(js) < 50_000 {
		t.Errorf("the JS bundle is implausibly small (%d bytes)", len(js))
	}
}

// TestNoThirdPartyAssetsAtRuntime: the UI is embedded in the binary and has to
// work air-gapped, so nothing may be fetched from a CDN or a font host.
func TestNoThirdPartyAssetsAtRuntime(t *testing.T) {
	html := readAsset(t, "index.html")
	for _, bad := range []string{
		"fonts.googleapis.com", "fonts.gstatic.com",
		"//cdn.", "//unpkg.com", "//cdnjs.", "//jsdelivr", "https://esm.sh",
	} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html references %q", bad)
		}
	}
	// Every script/stylesheet reference must be relative.
	for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		ref := m[1]
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") || strings.HasPrefix(ref, "//") {
			t.Errorf("index.html loads an absolute asset URL %q", ref)
		}
	}
}

// TestThemeAppliedBeforeFirstPaint: the theme is read from localStorage before
// first paint, so the page never flashes the wrong one — React mounts far too
// late to do it.
//
// The bootstrap must be an EXTERNAL script. The server sends
// script-src 'self' with no 'unsafe-inline', so an inline one is silently
// blocked: the attribute never lands, every :root[data-theme=…] selector stops
// matching, and the theme icons all disappear. That is exactly the bug this
// rewrite shipped for one build.
func TestThemeAppliedBeforeFirstPaint(t *testing.T) {
	html := readAsset(t, "index.html")

	if !strings.Contains(html, `src="./theme-init.js"`) {
		t.Error("index.html does not load the theme bootstrap")
	}
	head := html
	if i := strings.Index(html, "</head>"); i >= 0 {
		head = html[:i]
	}
	if !strings.Contains(head, "theme-init.js") {
		t.Error("the theme bootstrap must be in <head>, before the body renders")
	}
	// No inline <script> anywhere: CSP would block it.
	for _, m := range regexp.MustCompile(`(?s)<script(?:\s[^>]*)?>(.*?)</script>`).FindAllStringSubmatch(html, -1) {
		if strings.TrimSpace(m[1]) != "" {
			t.Errorf("index.html contains an inline script, which the CSP blocks: %.60s", strings.TrimSpace(m[1]))
		}
	}
	init := readAsset(t, "theme-init.js")
	if !strings.Contains(init, `localStorage.getItem("omnilog_theme")`) {
		t.Error("theme-init.js does not read the saved theme")
	}
	_, css := bundle(t)
	// The minifier strips quotes from attribute selectors, so match either form.
	if !regexp.MustCompile(`\[data-theme=["']?dark["']?\]`).MatchString(css) {
		t.Error("the stylesheet has no dark-theme block")
	}
	if !strings.Contains(css, "prefers-color-scheme") {
		t.Error("the stylesheet is missing prefers-color-scheme")
	}
}

// TestDarkThemeIsNeutral pins the palette decision: the dark ground is a real
// black rather than a blue-tinted near-black, and the chrome carries no hue at
// all. Colour is reserved for severity, so a coloured pixel means something.
func TestDarkThemeIsNeutral(t *testing.T) {
	_, css := bundle(t)

	for _, gone := range []string{"#0E1117", "#161B24", "#2348E0", "#6B8AFF"} {
		if strings.Contains(strings.ToUpper(css), strings.ToUpper(gone)) {
			t.Errorf("the stylesheet still contains the blue-tinted token %q", gone)
		}
	}

	// Only the severity variables may have hue. Minified CSS is one long line,
	// so scan declaration by declaration rather than by line.
	decl := regexp.MustCompile(`(--[a-z0-9-]+)\s*:\s*(#[0-9A-Fa-f]{6})`)
	allowed := regexp.MustCompile(`^--(error|warn|fatal|info|debug|ok|banner)`)
	matches := decl.FindAllStringSubmatch(css, -1)
	// Without this the test would pass simply by finding nothing to inspect —
	// which is exactly what happens if the minifier changes how it emits custom
	// properties.
	if len(matches) < 20 {
		t.Fatalf("only %d colour declarations found; the scan is not seeing the palette", len(matches))
	}
	for _, m := range matches {
		name, hex := m[1], m[2]
		if allowed.MatchString(name) {
			continue
		}
		r, _ := strconv.ParseInt(hex[1:3], 16, 0)
		g, _ := strconv.ParseInt(hex[3:5], 16, 0)
		b, _ := strconv.ParseInt(hex[5:7], 16, 0)
		if r != g || g != b {
			t.Errorf("non-neutral chrome colour %s: %s — chrome must be greyscale", name, hex)
		}
	}
}

// TestThirdPartyLicencesShip: the bundle is minified, which strips the banner
// comments the MIT licences of React and uPlot require to travel with the code.
// The notices are emitted beside the bundle instead, so they must be embedded.
func TestThirdPartyLicencesShip(t *testing.T) {
	notices := readAsset(t, "THIRD-PARTY.txt")
	for _, want := range []string{"react", "uplot", "MIT", "Permission is hereby granted"} {
		if !strings.Contains(strings.ToLower(notices), strings.ToLower(want)) {
			t.Errorf("THIRD-PARTY.txt is missing %q", want)
		}
	}
}

// TestExportDoesNotLeakTokenInURL guards that exports send the admin token in a
// header and save from a Blob, rather than appending it to the URL where it
// would reach browser history, Referer headers and proxy access logs. The
// live-tail EventSource still legitimately uses ?token= — it cannot set headers.
func TestExportDoesNotLeakTokenInURL(t *testing.T) {
	js, _ := bundle(t)
	if !strings.Contains(js, "createObjectURL") {
		t.Error("the export path no longer saves via a Blob URL")
	}
	if regexp.MustCompile(`token=.{0,12}(export|format=)`).MatchString(js) {
		t.Error("an export URL appears to carry the admin token as a query parameter")
	}
}

// TestUIFunctionalityIsPresent is a coarse check that the views actually made it
// into the bundle — a build that silently dropped a route would still produce
// plausible-looking output.
func TestUIFunctionalityIsPresent(t *testing.T) {
	js, _ := bundle(t)
	for _, want := range []string{
		"/api/v1/search", "/api/v1/search/stats", "/api/v1/aggregate", "/api/v1/tail",
		"/api/v1/alerts", "/api/v1/alerts/channels", "/api/v1/config", "/api/v1/export",
		"Live tail", "Overview", "Command palette", "drag to zoom a time range",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the bundle is missing %q", want)
		}
	}
}
