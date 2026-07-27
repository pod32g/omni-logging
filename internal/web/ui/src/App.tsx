import { useCallback, useEffect, useRef, useState } from "react";
import type { ViewName } from "./types";
import { setToken } from "./api";
import { AppBar } from "./components/AppBar";
import { CommandPalette, type Command } from "./components/CommandPalette";
import { Overview } from "./views/Overview";
import { Search, type SearchHandle } from "./views/Search";
import { LiveTail } from "./views/LiveTail";
import { Alerts } from "./views/Alerts";
import { Settings } from "./views/Settings";

const THEME_ORDER = ["system", "light", "dark"];

function initialTheme(): string {
  return document.documentElement.dataset.theme || "system";
}

export function App() {
  const [view, setView] = useState<ViewName>("dash");
  const [theme, setThemeState] = useState(initialTheme);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [tokenBar, setTokenBar] = useState(false);
  const [tokenDraft, setTokenDraft] = useState("");
  const searchRef = useRef<SearchHandle | null>(null);

  const setTheme = useCallback((t: string) => {
    document.documentElement.dataset.theme = t;
    try { localStorage.setItem("omnilog_theme", t); } catch { /* ignore */ }
    setThemeState(t);
  }, []);

  const goSearch = useCallback((q: string, range?: string) => {
    setView("search");
    // The view may be mounting for the first time, so let it commit before
    // driving it.
    queueMicrotask(() => searchRef.current?.setQuery(q, range));
  }, []);

  // Global keys. Anything typed into a field is left alone, so these never
  // steal a keystroke from the query box.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      const inField = !!t && ["INPUT", "TEXTAREA", "SELECT"].includes(t.tagName);

      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPaletteOpen(true);
        return;
      }
      if (e.key === "Escape") {
        if (paletteOpen) { setPaletteOpen(false); return; }
        if (inField) t!.blur();
        return;
      }
      if (inField || paletteOpen) return;

      if (e.key === "/") {
        e.preventDefault();
        setView("search");
        queueMicrotask(() => window.dispatchEvent(new Event("omnilog:focus-search")));
        return;
      }
      if (e.key === "g") {
        const onNext = (ev: KeyboardEvent) => {
          const map: Record<string, ViewName> = { o: "dash", s: "search", t: "tail", a: "alerts", c: "settings" };
          if (map[ev.key]) { ev.preventDefault(); setView(map[ev.key]); }
          window.removeEventListener("keydown", onNext, true);
        };
        window.addEventListener("keydown", onNext, true);
        setTimeout(() => window.removeEventListener("keydown", onNext, true), 900);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [paletteOpen]);

  const commands: Command[] = [
    { kind: "go", label: "Overview", run: () => setView("dash") },
    { kind: "go", label: "Search", run: () => setView("search") },
    { kind: "go", label: "Live tail", run: () => setView("tail") },
    { kind: "go", label: "Alerts", run: () => setView("alerts") },
    { kind: "go", label: "Settings", run: () => setView("settings") },
    { kind: "filter", label: "Only errors", run: () => goSearch("level=(error,fatal)") },
    { kind: "filter", label: "Only warnings", run: () => goSearch("level=warn") },
    { kind: "filter", label: "Clear the query", run: () => goSearch("") },
    { kind: "act", label: "Toggle theme", run: () => setTheme(THEME_ORDER[(THEME_ORDER.indexOf(theme) + 1) % 3]) },
  ];

  const onUnauthorized = useCallback(() => setTokenBar(true), []);

  return (
    <>
      <AppBar
        view={view}
        theme={theme}
        onNav={setView}
        onCycleTheme={() => setTheme(THEME_ORDER[(THEME_ORDER.indexOf(theme) + 1) % 3])}
        onOpenPalette={() => setPaletteOpen(true)}
        onToggleToken={() => setTokenBar((v) => !v)}
      />

      {tokenBar && (
        <div className="token-bar show" id="token-bar">
          <span>This server requires an admin token to query logs.</span>
          <input
            id="token-input" type="password" placeholder="admin token" value={tokenDraft}
            onChange={(e) => setTokenDraft(e.target.value)}
          />
          <button id="token-save" onClick={() => { setToken(tokenDraft.trim()); setTokenBar(false); location.reload(); }}>
            Save
          </button>
        </div>
      )}

      {/* Views stay mounted so switching back does not refetch or lose scroll;
          each is told whether it is the active one. */}
      <div className="view-host" hidden={view !== "dash"}>
        <Overview active={view === "dash"} theme={theme} onUnauthorized={onUnauthorized} onSearch={goSearch} />
      </div>
      <div className="view-host" hidden={view !== "search"}>
        <Search active={view === "search"} theme={theme} onUnauthorized={onUnauthorized} handleRef={searchRef} />
      </div>
      <div className="view-host" hidden={view !== "tail"}>
        <LiveTail active={view === "tail"} onFilter={(t) => goSearch(t)} />
      </div>
      <div className="view-host" hidden={view !== "alerts"}>
        <Alerts active={view === "alerts"} onUnauthorized={onUnauthorized} />
      </div>
      <div className="view-host" hidden={view !== "settings"}>
        <Settings active={view === "settings"} theme={theme} onTheme={setTheme} onUnauthorized={onUnauthorized} />
      </div>

      <CommandPalette
        open={paletteOpen}
        commands={commands}
        onClose={() => setPaletteOpen(false)}
        onSearch={(term) => goSearch(term)}
      />
    </>
  );
}
