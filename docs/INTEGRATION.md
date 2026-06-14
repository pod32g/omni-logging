# Integrating your services with Omni-logging

This guide covers how to ship logs from your applications and containers into a
running Omni-logging server.

In **v1 there are two ingestion paths**:

1. **Direct HTTP ingest** — your app POSTs structured JSON (best fidelity).
2. **The file forwarder** — `omnilog forward` tails log files and ships them.

> Native **syslog**, **OpenTelemetry/OTLP**, **Fluent/Beats** receivers and
> language **SDKs** are not in v1 — they're on the [roadmap](../ROADMAP.md)
> (M11–M13, M40). Until then, use HTTP ingest or the forwarder below.

---

## 1. Concepts

**Endpoints** (served by `omnilog serve`, default `:8080`):

| Endpoint | Body | Use |
|---|---|---|
| `POST /api/v1/ingest` | NDJSON (one JSON object per line) or a JSON array | structured logs |
| `POST /api/v1/ingest/raw` | `text/plain`, one log line per line | unstructured/plain text |

**Auth.** If the server was started with ingest keys (`--ingest-key …` or
`OMNILOG_INGEST_KEYS`), send the key as `X-Api-Key: <key>`. If no keys are
configured, ingest is open (fine on a trusted network).

**The canonical event.** Each ingested record becomes a `LogEvent`. For
structured ingest these keys (and common aliases) map onto first-class fields;
**any other key folds into `attributes`**:

| Field | Accepted keys | Notes |
|---|---|---|
| message | `message`, `msg` | the human-readable line |
| level | `level`, `severity`, `lvl` | normalized to `debug/info/warn/error/fatal` (also accepts `warning`, `err`, `critical`, syslog numbers 0–7) |
| service | `service`, `logger` | logical service name |
| source | `source`, `host`, `hostname` | origin host; for `/raw` it defaults to the client IP |
| timestamp | `timestamp`, `time`, `ts`, `@timestamp` | RFC3339 **or** unix seconds/millis/nanos (auto-detected); defaults to receipt time |
| attributes | `attributes` (object) + any unrecognized top-level keys | arbitrary structured fields |

**Response.** `{"accepted":N,"rejected":M,"errors":[{"index":i,"error":"…"}]}`.
Malformed records are rejected individually. If the server's ingest buffer is
full you get **HTTP 429** — back off and retry (never drop silently).

---

## 2. Path A — Direct HTTP ingest (recommended)

Send one JSON object per line (NDJSON). Unknown fields become searchable
attributes, so just include whatever structured context you have.

### curl

```sh
curl -fsS -XPOST http://HOST:8080/api/v1/ingest \
  -H 'X-Api-Key: devkey' \
  -H 'Content-Type: application/x-ndjson' \
  --data-binary $'{"service":"checkout-api","level":"error","message":"payment failed","order_id":"o_55120","status":504}\n'
```

`order_id` and `status` aren't reserved keys, so they're stored as attributes
and become filterable: `attr.order_id=o_55120` or `attr.status=504`.

### Go (`log/slog` handler)

A minimal batching JSON handler — buffer events and POST them asynchronously so
logging never blocks your request path:

```go
// Each record: map keys service/level/message/timestamp; extras -> attributes.
func ship(events []map[string]any, server, key string) error {
    var b bytes.Buffer
    enc := json.NewEncoder(&b)
    for _, e := range events {
        enc.Encode(e) // NDJSON: one object per line
    }
    req, _ := http.NewRequest("POST", server+"/api/v1/ingest", &b)
    req.Header.Set("Content-Type", "application/x-ndjson")
    if key != "" {
        req.Header.Set("X-Api-Key", key)
    }
    resp, err := http.DefaultClient.Do(req)
    if err != nil { return err }
    resp.Body.Close()
    if resp.StatusCode == 429 { /* buffer full: retry with backoff */ }
    return nil
}
```

### Python (`logging` handler)

```python
import json, logging, urllib.request

class OmniHandler(logging.Handler):
    def __init__(self, server, service, api_key=None):
        super().__init__()
        self.url = server.rstrip("/") + "/api/v1/ingest"
        self.service, self.api_key = service, api_key

    def emit(self, record):
        evt = {
            "service": self.service,
            "level": record.levelname,            # INFO/WARNING/ERROR -> normalized
            "message": record.getMessage(),
            "timestamp": record.created,           # unix seconds, auto-detected
            "logger_name": record.name,            # extra -> attribute
        }
        data = (json.dumps(evt) + "\n").encode()
        req = urllib.request.Request(self.url, data=data,
              headers={"Content-Type": "application/x-ndjson",
                       **({"X-Api-Key": self.api_key} if self.api_key else {})})
        try:
            urllib.request.urlopen(req, timeout=5).read()
        except Exception:
            self.handleError(record)              # never crash the app on a log

# logging.getLogger().addHandler(OmniHandler("http://HOST:8080", "f1-analyzer", "devkey"))
```

> For production, wrap this in a `QueueHandler`/`QueueListener` (or batch in a
> background thread) so logging is non-blocking. The example is intentionally
> minimal.

### Node.js (pino transport / fetch)

```js
async function ship(events, server, key) {
  const body = events.map((e) => JSON.stringify(e)).join("\n") + "\n";
  const res = await fetch(`${server}/api/v1/ingest`, {
    method: "POST",
    headers: { "Content-Type": "application/x-ndjson", ...(key && { "X-Api-Key": key }) },
    body,
  });
  if (res.status === 429) {/* buffer full: retry with backoff */}
}
// ship([{ service: "fansly", level: "info", message: "started", build: "1.4.2" }],
//      "http://HOST:8080", "devkey");
```

---

## 3. Path B — The file forwarder

For services that already write log files (or that you can't modify), run the
bundled forwarder. It tails files (rotation-aware, batched, retrying) and ships
each line to `/api/v1/ingest/raw`.

```sh
omnilog forward \
  --server http://HOST:8080 \
  --api-key devkey \
  --service nginx \
  --file /var/log/nginx/access.log \
  --file /var/log/nginx/error.log
```

Flags: `--source` (defaults to hostname), `--from-start` (forward existing
content first), `--batch` (lines per request, default 200). Each line is stored
as the `message`/`raw` of an event tagged with `--service`/`--source`.

Run it as a **systemd service** so it survives reboots:

```ini
# /etc/systemd/system/omnilog-forward-nginx.service
[Unit]
Description=Omni-logging forwarder (nginx)
After=network-online.target
[Service]
ExecStart=/usr/local/bin/omnilog forward --server http://HOST:8080 --api-key devkey --service nginx --file /var/log/nginx/access.log
Restart=always
[Install]
WantedBy=multi-user.target
```

---

## 4. Dockerized services (your setup)

Your services run as containers on `runner-host`. A few options, best first:

### 4a. App posts directly (preferred)
Use Path A from inside the app. Networking options for reaching omnilog from
another container:

- **Shared docker network** (cleanest). Put omnilog on a named external network
  and attach your services to it, then use the service name as host:
  ```sh
  docker network create omnilog-net
  # in omnilog's compose: networks: [omnilog-net]  (external: true)
  # in your app's compose:  networks: [omnilog-net]
  # app then POSTs to  http://omnilog:8080/api/v1/ingest
  ```
- **Via the host**: reach the published port at `http://<runner-host-LAN-or-tailscale-ip>:8080`
  (e.g. the Tailscale `jump-host`/`runner-host` address), or the docker host gateway
  (`host.docker.internal` on Docker Desktop; on Linux add
  `extra_hosts: ["host.docker.internal:host-gateway"]`).

### 4b. Forward a mounted log file
If the container writes logs to a file on a mounted volume, run `omnilog forward`
(Path B) against that path on the host, or as a small sidecar that mounts the
same volume.

### 4c. Containers that only log to stdout
Docker writes stdout to `/var/lib/docker/containers/<id>/<id>-json.log`, but each
line is wrapped (`{"log":"…","stream":"stdout","time":"…"}`). You *can* forward
that file, but lines arrive as the raw wrapper text (searchable, not
field-extracted). The clean fix is **4a** (post structured from the app) until
the native **fluentd/syslog/OTLP receivers** land
([roadmap](../ROADMAP.md) M11–M13), which will let Docker's logging drivers
target omnilog directly.

---

## 5. Verify it's working

```sh
# Recent logs from a service
curl -fsS "http://HOST:8080/api/v1/search?q=service=checkout-api&last=15m"

# Live tail (SSE), filtered
curl -N "http://HOST:8080/api/v1/tail?q=level=error"
```

Or open the web UI at `http://HOST:8080`, search `service=<your-service>`, and
switch to **Live tail** while your app emits a log line.

---

## 6. Good practices

- **Batch** with NDJSON (many objects per POST) rather than one request per line.
- **Never block the app** on logging — buffer/queue and ship asynchronously.
- **Handle 429** (buffer full) with a short backoff; the forwarder already does.
- **Put real structure in `attributes`** (ids, status codes, durations) — they're
  individually filterable (`attr.key=value`) and full-text searchable.
- **Use ISO-8601/RFC3339 or unix timestamps**; omit the field to use receipt time.
- **Set ingest keys** (`OMNILOG_INGEST_KEYS`) when the server isn't on a fully
  trusted network, and give each source its own key.
