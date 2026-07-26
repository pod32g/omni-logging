# Client SDKs

Thin, dependency-free clients so an application can emit to Omni-logging
directly, without the file forwarder in between.

| Language | Path | Framework integration |
|---|---|---|
| Go | [`sdk/omnilog`](omnilog) | `log/slog` handler |
| Python | [`sdk/python/omnilog.py`](python/omnilog.py) | `logging.Handler` |
| JavaScript | [`sdk/js/omnilog.mjs`](js/omnilog.mjs) | pino transport |

All three share the same design:

- **Logging never blocks on the network.** Events are batched and delivered on
  a background worker. If the queue fills because the server is slow or down,
  events are dropped and counted rather than the caller being stalled — an
  application must not grind to a halt because its logging backend is unwell.
- **Delivery counters are exposed** (`sent`, `failed`, `dropped`) so you can
  alert on your own logging pipeline instead of discovering the loss later.
- **Any extra field becomes a searchable attribute**, so
  `log.error("failed", {status: 402})` is queryable as `attr.status>=400`
  immediately.
- **Optional gzip**, which typically cuts log payloads to a few percent.
- **No dependencies** beyond each language's standard library.

## A note on reserved field names

The server maps a handful of top-level keys onto first-class event fields:

```
id  timestamp  time  ts  @timestamp  received_at
source  host  hostname  service  logger  level  severity  lvl
message  msg  raw  attributes
```

An attribute using one of those names is consumed as that field rather than
stored — `logger`, for instance, is an alias for `service`. Name your own
fields something else; the Python handler ships the logger name as
`logger_name` for exactly this reason.

## Go

```go
client, _ := omnilog.New(omnilog.Options{
    ServerURL: "http://logs:8080", APIKey: "devkey", Service: "api", Compress: true,
})
defer client.Close()

slog.SetDefault(slog.New(omnilog.NewHandler(client, &omnilog.HandlerOptions{
    Fallback: slog.NewTextHandler(os.Stderr, nil), // keep local output too
})))

slog.Error("payment failed", "status", 402, "retryable", true)
```

`slog` groups become dotted attribute names (`http.status`), because the event
model is flat and that is the path the query language reaches.

## Python

```python
handler = omnilog.OmnilogHandler(
    server_url="http://logs:8080", api_key="devkey", service="api", compress=True)
logging.getLogger().addHandler(handler)

logging.getLogger("checkout").error("payment failed", extra={"status": 402})
```

## JavaScript

```js
import { Omnilog } from './omnilog.mjs';

const log = new Omnilog({ serverUrl: 'http://logs:8080', apiKey: 'devkey', service: 'api' });
log.error('payment failed', { status: 402, retryable: true });
await log.close();

// or as a pino transport
const transport = pino.transport({ target: './omnilog.mjs', options: { serverUrl: '...' } });
```
