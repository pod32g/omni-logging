// Omni-logging client for Node.js. No dependencies: it uses the built-in
// fetch and zlib only.
//
//   import { Omnilog } from './omnilog.mjs';
//   const log = new Omnilog({ serverUrl: 'http://logs:8080', apiKey: 'devkey', service: 'api' });
//   log.error('payment failed', { status: 402, retryable: true });
//   await log.close();
//
// Events are batched and flushed on a timer, so a log call never awaits the
// network. When the queue fills the oldest events are dropped and counted
// rather than the caller being blocked: an application must not stall because
// its logging backend is unwell.

import { gzipSync } from 'node:zlib';

const LEVELS = ['debug', 'info', 'warn', 'error', 'fatal'];

export class Omnilog {
  /**
   * @param {object} opts
   * @param {string} opts.serverUrl      base URL, e.g. http://localhost:8080
   * @param {string} [opts.apiKey]       sent as X-Api-Key
   * @param {string} [opts.service]      default service for every event
   * @param {string} [opts.source]       default source for every event
   * @param {number} [opts.batchSize]    flush once this many are queued
   * @param {number} [opts.flushInterval] ms between timed flushes
   * @param {number} [opts.queueSize]    max queued events before dropping
   * @param {boolean} [opts.compress]    gzip each batch
   * @param {(e: Error) => void} [opts.onError] delivery failure callback
   */
  constructor(opts = {}) {
    if (!opts.serverUrl) throw new Error('omnilog: serverUrl is required');
    this.url = opts.serverUrl.replace(/\/+$/, '') + '/api/v1/ingest';
    this.apiKey = opts.apiKey || '';
    this.service = opts.service || '';
    this.source = opts.source || '';
    this.batchSize = opts.batchSize ?? 100;
    this.flushInterval = opts.flushInterval ?? 2000;
    this.queueSize = opts.queueSize ?? 10000;
    this.compress = opts.compress ?? false;
    this.onError = opts.onError ?? (() => {});
    this.timeoutMs = opts.timeoutMs ?? 10000;

    this.queue = [];
    this.stats = { sent: 0, failed: 0, dropped: 0 };
    this.closed = false;
    this.inflight = Promise.resolve();

    this.timer = setInterval(() => { this.flush(); }, this.flushInterval);
    // Do not hold the process open just because a logger is running.
    if (typeof this.timer.unref === 'function') this.timer.unref();
  }

  /** Queue one event. Never awaits; drops (and counts) when the queue is full. */
  send(event) {
    if (this.closed) return;
    if (this.queue.length >= this.queueSize) {
      // Drop the oldest: during a burst the newest events are the ones that
      // describe what is happening now.
      this.queue.shift();
      this.stats.dropped++;
    }
    const e = { service: this.service, source: this.source, ...event };
    for (const k of Object.keys(e)) {
      if (e[k] === '' || e[k] === undefined || e[k] === null) delete e[k];
    }
    this.queue.push(e);
    if (this.queue.length >= this.batchSize) this.flush();
  }

  /** Queue one event from its parts; attrs become searchable attributes. */
  log(level, message, attrs = {}) {
    this.send({ level, message, timestamp: new Date().toISOString(), ...attrs });
  }

  /** Flush queued events. Returns a promise resolving when delivery settles. */
  flush() {
    if (this.queue.length === 0) return this.inflight;
    const batch = this.queue;
    this.queue = [];
    // Chain so batches are delivered in order rather than racing each other.
    this.inflight = this.inflight.then(() => this.#post(batch));
    return this.inflight;
  }

  /** Flush and stop the timer. Safe to call twice. */
  async close() {
    if (this.closed) return;
    this.closed = true;
    clearInterval(this.timer);
    await this.flush();
    await this.inflight;
  }

  async #post(batch) {
    const ndjson = batch.map((e) => JSON.stringify(e)).join('\n');
    const headers = { 'Content-Type': 'application/x-ndjson' };
    if (this.apiKey) headers['X-Api-Key'] = this.apiKey;

    let body = Buffer.from(ndjson, 'utf8');
    if (this.compress) {
      body = gzipSync(body);
      headers['Content-Encoding'] = 'gzip';
    }

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeoutMs);
    try {
      const res = await fetch(this.url, {
        method: 'POST', headers, body, signal: controller.signal,
      });
      if (!res.ok) {
        const text = await res.text().catch(() => '');
        throw new Error(`omnilog: server returned ${res.status}: ${text.slice(0, 200)}`);
      }
      this.stats.sent += batch.length;
    } catch (err) {
      this.stats.failed += batch.length;
      // A logging client must never reject into the application.
      try { this.onError(err); } catch { /* ignore */ }
    } finally {
      clearTimeout(timer);
    }
  }
}

// Level shorthands: log.error(...), log.info(...), and so on.
for (const level of LEVELS) {
  Omnilog.prototype[level] = function (message, attrs) {
    this.log(level, message, attrs);
  };
}

/**
 * A pino transport. Point pino at this module and its records ship with no
 * further wiring:
 *
 *   const transport = pino.transport({ target: './omnilog.mjs', options: {...} });
 *   const log = pino(transport);
 */
export default function pinoTransport(options = {}) {
  const client = new Omnilog(options);
  const PINO_LEVELS = { 10: 'debug', 20: 'debug', 30: 'info', 40: 'warn', 50: 'error', 60: 'fatal' };

  return async function* (source) {
    for await (const obj of source) {
      const { level, time, msg, hostname, pid, ...rest } = obj;
      client.send({
        timestamp: time ? new Date(time).toISOString() : new Date().toISOString(),
        level: PINO_LEVELS[level] || 'info',
        message: msg ?? '',
        source: hostname,
        pid,
        ...rest,
      });
    }
    await client.close();
  };
}
