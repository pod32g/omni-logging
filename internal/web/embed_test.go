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

// TestHistogramBarsAreBoundedByTheirContainer pins the fix for a real collision:
// bar heights were computed in app.js as a pixel constant that had to be kept in
// step with the .bars height in styles.css. When the CSS shrank to 46px the 62px
// bars overflowed upward and painted over the panel header — 24 overlapping
// element pairs. Sizing in percent makes the container the only source of truth,
// so the two files cannot disagree again.
func TestHistogramBarsAreBoundedByTheirContainer(t *testing.T) {
	js := readAsset(t, "app.js")

	i := strings.Index(js, "norm.style.height")
	if i < 0 {
		t.Fatal("app.js no longer sets a bar height; update this test with the new mechanism")
	}
	stmt := js[i:]
	if end := strings.Index(stmt, "\n"); end >= 0 {
		stmt = stmt[:end]
	}
	if !strings.Contains(stmt, `+ "%"`) {
		t.Errorf("bar height is not relative to its container: %q", strings.TrimSpace(stmt))
	}
	if strings.Contains(stmt, `+ "px"`) {
		t.Errorf("bar height is back to absolute pixels, which must track the CSS by hand: %q", strings.TrimSpace(stmt))
	}

	// And the plot area clips, so nothing can paint outside it regardless.
	css := readAsset(t, "styles.css")
	if !strings.Contains(css, ".bars { display: flex; align-items: flex-end; gap: 1px; height: 46px; overflow: hidden; }") {
		t.Error("styles.css: .bars must clip its contents so an oversized bar cannot escape")
	}
}
