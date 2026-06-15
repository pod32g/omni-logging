# Settings page + editable server config (delivers M5 config hot-reload)

*Roadmap M5 · Depends on M4. User-requested Settings UI + editable server config.*

## Goal
A Settings page that edits mutable server config live, persisted across restarts
and hot-applied without dropping connections — i.e. M5's config hot-reload, driven
by an admin API + UI instead of SIGHUP.

## Editable (mutable) settings
`retention_days`, `rate_limit_per_sec`, `rate_burst`, `daily_quota_events`,
`daily_quota_bytes`, `log_level`, `ingest_keys`. **Admin token is intentionally
NOT editable from the UI** (browser-edit lockout risk) — file/env/flag only.

## Persistence & precedence
Migration **v3** adds a `settings(key TEXT PRIMARY KEY, value TEXT)` table. The
mutable set is stored as one JSON row (`key="mutable"`). Effective config =
defaults → file → env → flags → **DB overrides** (highest, for the mutable subset
only). So once edited via the UI, that value persists and wins over file/flags;
documented behavior.

## Components
- **`internal/settings.Manager`**: holds the current `Mutable` behind a RWMutex,
  loads the DB overlay on top of the startup base, and on `Apply` validates →
  persists → fires change hooks. Live getters (`Current`, `IngestKeys`,
  `RetentionDays`) for pull consumers.
- **`SettingsStore`** (sqlite `GetSettings`/`PutSettings`, not on the generic
  `store.Store` interface — passed to the Manager and api).
- **Hot-apply**: push hooks update the admission limiter (`Limiter.SetLimits`) and
  the slog level (`*slog.LevelVar`); pull consumers read live values — the
  retention goroutine reads `RetentionDays()` each tick (always running; 0 = skip),
  and `requireIngestKey` reads `IngestKeys()` per request.

## API (admin-auth)
- `GET /api/v1/config` → current mutable settings (JSON).
- `PUT /api/v1/config` → replace the mutable set (validated), persist + hot-apply,
  return the new effective settings. Validation: non-negative numbers, known log
  level, ingest keys trimmed/de-duped.

## UI
New `Settings` nav item + `#view-settings` with sections: Appearance (theme),
Server config form (retention / rate limit / quotas / log level), Ingest keys
(list + add/remove), Connection (admin token — client-side browser auth, the
existing token bar), and read-only Server status (version, schema version, health).
Save → `PUT /api/v1/config`.

## Testing (TDD)
- sqlite: settings get/put + migration v3 (table exists, round-trip).
- `settings.Manager`: overlay load, Apply persists + fires hooks + updates getters,
  validation rejects bad input, concurrency (`-race`).
- `admission.Limiter.SetLimits`: takes effect on next Allow.
- api: `GET`/`PUT /api/v1/config` (admin-gated; PUT changes a live limit and a
  subsequent ingest reflects it; persisted value reloads).
- Runtime: edit a setting via the API, confirm hot-apply + persistence across restart.

## Definition of done
`gofmt`/`vet`/`go test -race ./...` green; editable config verified live + across
restart; UI rebuilt + embedded; README updated. Admin token never editable via UI.
No data-loss risk (settings table additive). No new third-party dependency.
