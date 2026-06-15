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
	for _, want := range []string{`id="theme-toggle"`, `localStorage.getItem("omnilog_theme")`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
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
