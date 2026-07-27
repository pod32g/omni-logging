import type { ViewName } from "../types";

const NAV: { id: ViewName; label: string }[] = [
  { id: "dash", label: "Overview" },
  { id: "search", label: "Search" },
  { id: "tail", label: "Live tail" },
  { id: "alerts", label: "Alerts" },
  { id: "settings", label: "Settings" },
];

interface Props {
  view: ViewName;
  theme: string;
  onNav: (v: ViewName) => void;
  onCycleTheme: () => void;
  onOpenPalette: () => void;
  onToggleToken: () => void;
}

export function AppBar({ view, theme, onNav, onCycleTheme, onOpenPalette, onToggleToken }: Props) {
  return (
    <header className="appbar">
      <div className="brand">
        <div className="mark"><span /><span /><span /></div>
        <div className="wordmark"><strong>Omni</strong><span>logging</span></div>
      </div>
      <nav className="nav">
        {NAV.map((n) => (
          <button
            key={n.id}
            className={`nav-item${view === n.id ? " is-active" : ""}`}
            data-view={n.id}
            onClick={() => onNav(n.id)}
          >
            {n.label}
          </button>
        ))}
      </nav>
      <div className="account">
        <div className="env" id="env-pill"><i className="dot dot-ok" /><span>production</span></div>
        <button className="kbd-btn" id="palette-btn" title="Command palette (⌘K)" onClick={onOpenPalette}>
          <svg viewBox="0 0 24 24">
            <rect x="3" y="4" width="18" height="16" rx="2" />
            <path d="M7 9h.01M11 9h.01M15 9h.01M7 14h10" />
          </svg>
        </button>
        <button
          className="theme-toggle"
          id="theme-toggle"
          title={`Theme: ${theme} (click to change)`}
          onClick={onCycleTheme}
        >
          <svg className="ti ti-system" viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="13" rx="1.5" /><path d="M8 21h8M12 17v4" /></svg>
          <svg className="ti ti-light" viewBox="0 0 24 24"><circle cx="12" cy="12" r="4.5" /><path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.5 1.5M17.5 17.5L19 19M19 5l-1.5 1.5M6.5 17.5L5 19" /></svg>
          <svg className="ti ti-dark" viewBox="0 0 24 24"><path d="M21 13a8 8 0 11-9.5-9.5A6.5 6.5 0 0021 13z" /></svg>
        </button>
        <button className="avatar" id="token-btn" title="Set admin token" onClick={onToggleToken}>DI</button>
      </div>
    </header>
  );
}
