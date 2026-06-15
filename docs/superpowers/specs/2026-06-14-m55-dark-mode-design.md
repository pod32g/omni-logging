# M55 — Dark mode & theming (light / dark / system)

*Roadmap M55 · Effort S · Depends on v1. P0 comfort win.*

## Goal

A dark theme plus a light/dark/system toggle in the app bar, persisted to
`localStorage`, honoring `prefers-color-scheme` in "system" mode. Rebuild to embed.

## Approach

- `<html data-theme="light|dark|system">` drives theming. A tiny inline script in
  `<head>` sets it from `localStorage` **before first paint** (no flash of the
  wrong theme).
- CSS:
  - `:root` keeps the existing **light** values (default).
  - `:root[data-theme="dark"]` overrides the custom properties with a dark palette.
  - `@media (prefers-color-scheme: dark) { :root[data-theme="system"] { …dark… } }`
    so "system" follows the OS, defaulting to light otherwise.
- **Refactor hardcoded colors to variables.** The v1 CSS hardcodes several light
  hex values (`#fff` inputs/cards/chips, `#F7F9FB` column header, `#FAFBFD` row
  hover, `#39424F` message, the token banner) and uses `var(--ink)` as a *dark*
  background for the code block and avatar — which would invert wrongly in dark
  mode. Introduce semantic vars (`--elev`, `--input-border`, `--col-header-bg`,
  `--row-hover`, `--msg`, `--chip-border`, `--code-bg`, `--code-ink`, `--switch-off`,
  `--banner-*`) used by both palettes. The avatar/code block switch to vars that
  read correctly in both themes.

## Toggle (app bar)

A compact cycling button in `.account` (before the avatar) that rotates
system → light → dark → system. It contains three inline SVG icons
(monitor / sun / moon); CSS shows the one matching `html[data-theme]`. Click
updates `data-theme`, persists `omnilog_theme`, and the icon follows.

## Files

- `index.html`: head no-flash script; `#theme-toggle` button with three icons.
- `styles.css`: semantic-var refactor; dark palette; `.theme-toggle` styles.
- `app.js`: `initTheme()` + cycle-on-click handler.

## Verification

`go build` to embed; load the UI in a browser; toggle through all three modes and
confirm the palette switches and persists across reload; confirm "system" follows
`prefers-color-scheme`. Check key surfaces stay legible in dark: app bar, query
bar inputs, facets, histogram, result rows + expanded JSON block, badges, token
banner, live-tail.

## Definition of done

UI rebuilt + embedded; theme switches/persists/honors OS; no contrast regressions;
`go test ./...` still green (no Go behavior change). No new dependency.
