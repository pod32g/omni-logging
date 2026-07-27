package web

import (
	"io"
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

// TestEmbeddedUIHasThemeSupport guards that the dark-mode theming is actually
// embedded — i.e. the binary was rebuilt after the dist changes. It catches the
// easy mistake of editing dist/ without re-running `go build`.
func TestEmbeddedUIHasThemeSupport(t *testing.T) {
	html := readAsset(t, "index.html")
	for _, want := range []string{`id="theme-toggle"`, `src="theme-init.js"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Error("index.html still loads third-party font resources")
	}
	if initJS := readAsset(t, "theme-init.js"); !strings.Contains(initJS, `localStorage.getItem("omnilog_theme")`) {
		t.Error("theme-init.js missing saved-theme initialization")
	}
	css := readAsset(t, "styles.css")
	for _, want := range []string{`[data-theme="dark"]`, "prefers-color-scheme"} {
		if !strings.Contains(css, want) {
			t.Errorf("styles.css missing %q", want)
		}
	}
	if js := readAsset(t, "app.js"); !strings.Contains(js, "function setTheme") {
		t.Error("app.js missing setTheme")
	}
}

// TestEmbeddedUIHasExportAndPagination guards the M26 UI controls are embedded.
func TestEmbeddedUIHasExportAndPagination(t *testing.T) {
	html := readAsset(t, "index.html")
	for _, want := range []string{`id="load-more"`, `id="export-ndjson"`, `id="export-csv"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
}

// TestEmbeddedUIHasSettings guards the Settings view is embedded.
func TestEmbeddedUIHasSettings(t *testing.T) {
	html := readAsset(t, "index.html")
	for _, want := range []string{`data-view="settings"`, `id="view-settings"`, `id="cfg-save"`, `id="cfg-keys"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	if js := readAsset(t, "app.js"); !strings.Contains(js, "function loadSettings") {
		t.Error("app.js missing loadSettings")
	}
}

// TestExportDoesNotLeakTokenInURL guards that the export download fetches with
// the admin token in the Authorization header and saves via a Blob URL, rather
// than appending the token as a "&token=" query parameter (which would leak it
// into browser history, Referer headers, and upstream proxy access logs). The
// EventSource live tail still legitimately uses ?token= — it can't set headers —
// so this test targets only the export path's specific leak pattern.
func TestExportDoesNotLeakTokenInURL(t *testing.T) {
	js := readAsset(t, "app.js")
	if strings.Contains(js, `"&token=" + encodeURIComponent(t)`) {
		t.Error("app.js export download still appends the admin token to the URL query string")
	}
	if !strings.Contains(js, "createObjectURL") {
		t.Error("app.js export download missing Blob (URL.createObjectURL) save path")
	}
}

// TestEmbeddedUIHasOverviewAndPalette guards the redesigned shell is embedded:
// the Overview landing view, the command palette, and the filter strip that
// replaced the facets sidebar.
func TestEmbeddedUIHasOverviewAndPalette(t *testing.T) {
	html := readAsset(t, "index.html")
	for _, want := range []string{
		`data-view="dash"`, `id="view-dash"`, `id="dash-tiles"`,
		`id="palette"`, `id="pal-input"`, `id="filterbar"`, `id="bars-wrap"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	js := readAsset(t, "app.js")
	for _, want := range []string{"function loadDash", "function openPalette", "function setQueryRange"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}

// TestDarkThemeIsNeutral pins the palette decision: the dark ground is a real
// black rather than the blue-tinted near-black it used to be, and the interface
// carries no blue or purple chrome. Colour is reserved for severity, so a
// coloured pixel should mean something.
func TestDarkThemeIsNeutral(t *testing.T) {
	css := readAsset(t, "styles.css")

	// The old blue-tinted surfaces and cobalt accent must be gone.
	for _, gone := range []string{"#0E1117", "#161B24", "#2348E0", "#6B8AFF", "--cobalt"} {
		if strings.Contains(css, gone) {
			t.Errorf("styles.css still contains the blue-tinted token %q", gone)
		}
	}

	// Every hex colour outside the severity block must be a neutral grey —
	// equal R, G and B. A hue anywhere else is the thing this test exists to
	// catch.
	hex := regexp.MustCompile(`#([0-9A-Fa-f]{6})\b`)
	for _, line := range strings.Split(css, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		// Severity and the one status green are allowed to have hue.
		if strings.Contains(line, "--error") || strings.Contains(line, "--warn") ||
			strings.Contains(line, "--fatal") || strings.Contains(line, "--ok") ||
			strings.Contains(line, "--banner") {
			continue
		}
		for _, m := range hex.FindAllStringSubmatch(line, -1) {
			r, _ := strconv.ParseInt(m[1][0:2], 16, 0)
			g, _ := strconv.ParseInt(m[1][2:4], 16, 0)
			b, _ := strconv.ParseInt(m[1][4:6], 16, 0)
			if r != g || g != b {
				t.Errorf("non-neutral chrome colour %s in %q — chrome must be greyscale", m[0], trimmed)
			}
		}
	}
}

// TestHistogramUsesVendoredUPlot replaces an earlier test that pinned how bar
// heights were computed. That whole class of bug — a pixel size in app.js
// drifting out of step with a height in styles.css until bars painted over the
// panel header — is gone because the chart library owns the scale now. What is
// worth guarding instead is that the library is vendored: the UI is embedded in
// the binary and must work air-gapped, so a CDN reference would be a silent
// runtime dependency on the internet.
func TestHistogramUsesVendoredUPlot(t *testing.T) {
	html := readAsset(t, "index.html")
	for _, want := range []string{`href="vendor/uPlot.min.css"`, `src="vendor/uPlot.min.js"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// Every script and stylesheet must be same-origin.
	for _, bad := range []string{"//cdn.", "//unpkg.com", "//cdnjs.", "//jsdelivr", "https://esm.sh"} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html loads an off-origin asset (%q); the UI must work air-gapped", bad)
		}
	}

	// The vendored files must actually be embedded, not just referenced.
	js := readAsset(t, "vendor/uPlot.min.js")
	if len(js) < 20000 || !strings.Contains(js, "uPlot") {
		t.Errorf("vendor/uPlot.min.js looks wrong (%d bytes)", len(js))
	}
	if lic := readAsset(t, "vendor/uPlot.LICENSE"); !strings.Contains(lic, "MIT") {
		t.Error("vendor/uPlot.LICENSE missing or not the MIT text")
	}
	readAsset(t, "vendor/uPlot.min.css")

	app := readAsset(t, "app.js")
	if !strings.Contains(app, "new uPlot(") {
		t.Error("app.js does not construct a uPlot chart")
	}
	// Selection must hand the range to the query rather than zooming the chart,
	// which is what keeps the selected window visible and shareable.
	if !strings.Contains(app, "setScale: false") {
		t.Error("app.js: chart drag should select without rescaling; the range belongs in the query")
	}
	if strings.Contains(app, "norm.style.height") {
		t.Error("app.js still sizes bars by hand; uPlot owns the geometry now")
	}
}
