"""Omni-logging client and stdlib ``logging`` handler.

Standard library only — adding this to an application pulls in nothing.

    import logging, omnilog

    handler = omnilog.OmnilogHandler(
        server_url="http://logs:8080", api_key="devkey", service="api")
    logging.getLogger().addHandler(handler)
    logging.getLogger().info("hello", extra={"status": 200})

Records are batched and sent on a background thread, so a logging call never
blocks on the network. If the queue fills — because the server is slow or down —
records are dropped and counted rather than blocking the caller: an application
must not stall because its logging backend is unwell.
"""

from __future__ import annotations

import atexit
import gzip
import json
import logging
import queue
import threading
import time
import urllib.error
import urllib.request
from typing import Any, Callable, Dict, Iterable, Optional

__all__ = ["Client", "OmnilogHandler", "Stats"]

# Attributes the stdlib puts on every LogRecord. Anything outside this set was
# added by the caller (via `extra=`) and is worth shipping as a searchable
# attribute; shipping the rest would bury the useful fields in noise.
_STANDARD_RECORD_FIELDS = frozenset(
    """args asctime created exc_info exc_text filename funcName levelname levelno
    lineno module msecs message msg name pathname process processName
    relativeCreated stack_info thread threadName taskName""".split()
)

_LEVEL_NAMES = {
    logging.DEBUG: "debug",
    logging.INFO: "info",
    logging.WARNING: "warn",
    logging.ERROR: "error",
    logging.CRITICAL: "fatal",
}


def _level_name(levelno: int) -> str:
    """Map a numeric level onto the server's vocabulary.

    Levels are arbitrary integers in Python, so bucket by range rather than
    requiring an exact match on the well-known constants.
    """
    if levelno >= logging.CRITICAL:
        return "fatal"
    if levelno >= logging.ERROR:
        return "error"
    if levelno >= logging.WARNING:
        return "warn"
    if levelno >= logging.INFO:
        return "info"
    return "debug"


class Stats:
    """Delivery counters, so an application can alert on its own log pipeline."""

    __slots__ = ("sent", "failed", "dropped")

    def __init__(self, sent: int = 0, failed: int = 0, dropped: int = 0) -> None:
        self.sent = sent
        self.failed = failed
        self.dropped = dropped

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        return f"Stats(sent={self.sent}, failed={self.failed}, dropped={self.dropped})"


class Client:
    """Batches events and ships them to an Omni-logging server."""

    def __init__(
        self,
        server_url: str,
        api_key: str = "",
        service: str = "",
        source: str = "",
        batch_size: int = 100,
        flush_interval: float = 2.0,
        queue_size: int = 10000,
        compress: bool = False,
        timeout: float = 10.0,
        on_error: Optional[Callable[[Exception], None]] = None,
    ) -> None:
        if not server_url:
            raise ValueError("omnilog: server_url is required")

        self._url = server_url.rstrip("/") + "/api/v1/ingest"
        self._api_key = api_key
        self._service = service
        self._source = source
        self._batch_size = max(1, batch_size)
        self._flush_interval = max(0.01, flush_interval)
        self._compress = compress
        self._timeout = timeout
        self._on_error = on_error

        self._queue: "queue.Queue[Optional[Dict[str, Any]]]" = queue.Queue(maxsize=queue_size)
        self._stats = Stats()
        self._lock = threading.Lock()
        self._closed = threading.Event()
        self._worker = threading.Thread(target=self._run, name="omnilog", daemon=True)
        self._worker.start()
        atexit.register(self.close)

    # -- public API ---------------------------------------------------------

    def send(self, event: Dict[str, Any]) -> None:
        """Queue one event. Never blocks; drops and counts when full."""
        event.setdefault("service", self._service)
        event.setdefault("source", self._source)
        event = {k: v for k, v in event.items() if v not in ("", None)}
        try:
            self._queue.put_nowait(event)
        except queue.Full:
            with self._lock:
                self._stats.dropped += 1

    def log(self, level: str, message: str, **attrs: Any) -> None:
        """Queue one event from its parts."""
        event: Dict[str, Any] = {"level": level, "message": message}
        event.update(attrs)
        self.send(event)

    def stats(self) -> Stats:
        with self._lock:
            return Stats(self._stats.sent, self._stats.failed, self._stats.dropped)

    def close(self, timeout: float = 5.0) -> None:
        """Flush queued events and stop the worker. Safe to call twice."""
        if self._closed.is_set():
            return
        self._closed.set()
        try:
            self._queue.put_nowait(None)  # sentinel: drain and exit
        except queue.Full:
            pass
        self._worker.join(timeout=timeout)

    # -- worker -------------------------------------------------------------

    def _run(self) -> None:
        batch: list = []
        deadline = time.monotonic() + self._flush_interval
        while True:
            timeout = max(0.0, deadline - time.monotonic())
            try:
                item = self._queue.get(timeout=timeout)
            except queue.Empty:
                item = _TICK
            if item is None:  # shutdown sentinel
                self._flush(batch)
                # Drain anything queued between the sentinel and now.
                while True:
                    try:
                        extra = self._queue.get_nowait()
                    except queue.Empty:
                        break
                    if extra is not None:
                        batch.append(extra)
                self._flush(batch)
                return
            if item is not _TICK:
                batch.append(item)
            if len(batch) >= self._batch_size or time.monotonic() >= deadline:
                self._flush(batch)
                batch = []
                deadline = time.monotonic() + self._flush_interval

    def _flush(self, batch: list) -> None:
        if not batch:
            return
        payload = "\n".join(json.dumps(e, default=str) for e in batch).encode("utf-8")
        headers = {"Content-Type": "application/x-ndjson"}
        if self._api_key:
            headers["X-Api-Key"] = self._api_key
        if self._compress:
            payload = gzip.compress(payload)
            headers["Content-Encoding"] = "gzip"

        req = urllib.request.Request(self._url, data=payload, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=self._timeout) as resp:
                resp.read()
            with self._lock:
                self._stats.sent += len(batch)
        except Exception as exc:  # noqa: BLE001 - a logging client must not raise
            with self._lock:
                self._stats.failed += len(batch)
            if self._on_error is not None:
                try:
                    self._on_error(exc)
                except Exception:  # pragma: no cover - never let a callback escape
                    pass
        finally:
            batch.clear()


class _Tick:
    """Sentinel distinguishing a flush tick from a real event."""


_TICK = _Tick()


class OmnilogHandler(logging.Handler):
    """A ``logging.Handler`` that ships records to an Omni-logging server.

    Anything passed through ``extra=`` becomes a searchable attribute, so
    ``log.info("done", extra={"status": 200})`` is queryable as
    ``attr.status>=200`` with no further configuration.
    """

    def __init__(
        self,
        server_url: str = "",
        api_key: str = "",
        service: str = "",
        source: str = "",
        client: Optional[Client] = None,
        level: int = logging.NOTSET,
        include_fields: Iterable[str] = (),
        **client_kwargs: Any,
    ) -> None:
        super().__init__(level=level)
        if client is None:
            client = Client(
                server_url=server_url, api_key=api_key,
                service=service, source=source, **client_kwargs,
            )
            self._owns_client = True
        else:
            self._owns_client = False
        self._client = client
        # Standard record fields are dropped by default; name any you want kept.
        self._include_fields = frozenset(include_fields)

    @property
    def client(self) -> Client:
        return self._client

    def emit(self, record: logging.LogRecord) -> None:
        try:
            attrs: Dict[str, Any] = {
                k: v
                for k, v in record.__dict__.items()
                if k not in _STANDARD_RECORD_FIELDS or k in self._include_fields
            }
            # Deliberately not "logger": the server treats that key as an alias
            # for the service name, so a logger attribute would be swallowed
            # rather than stored. Verified against a running server.
            attrs["logger_name"] = record.name
            if record.exc_info:
                attrs["exception"] = self.format(record) if self.formatter else logging.Formatter().formatException(record.exc_info)

            event: Dict[str, Any] = {
                "timestamp": time.strftime(
                    "%Y-%m-%dT%H:%M:%S", time.gmtime(record.created)
                ) + f".{int(record.msecs):03d}Z",
                "level": _level_name(record.levelno),
                "message": record.getMessage(),
            }
            event.update(attrs)
            self._client.send(event)
        except Exception:  # noqa: BLE001
            # logging.Handler contract: report, never raise into the caller.
            self.handleError(record)

    def flush(self) -> None:
        # Nothing to force: the worker flushes on its own interval, and close()
        # drains. Overridden so logging.shutdown() does not error.
        pass

    def close(self) -> None:
        try:
            if self._owns_client:
                self._client.close()
        finally:
            super().close()
