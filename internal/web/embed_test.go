package web

import (
	"io"
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
