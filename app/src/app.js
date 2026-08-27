'use strict';

const express = require('express');
const os = require('os');
const { visitorMiddleware } = require('./visitor');
const { createRateLimiter } = require('./ratelimit');

/**
 * The value of the `route` metric label: the registered Express route, or one
 * constant for everything that matched nothing.
 *
 * It used to be `req.path`, the raw URL. Every distinct path a client invented
 * then created permanent series in the registry — one counter plus a histogram
 * with nine buckets, a _sum and a _count — and prom-client series never expire.
 * Measured on a running pod: about 2,300 made-up paths took the registry from
 * 14 series to 2,332 counters and 23,280 histogram buckets, and /metrics from
 * 14 KB to 2.9 MB. Prometheus kept them. The pod's memory limit is 192Mi and
 * the number of URLs a stranger can type is not bounded, so this was an
 * unauthenticated way to kill the process and bloat the TSDB behind it, needing
 * nothing but a loop over 404s. Restricting /metrics does not help — the series
 * are created by traffic to the application port.
 *
 * A registered route is a value this application chose, so the label is bounded
 * by the routing table. Anything else collapses to "unmatched", which is one
 * series no matter how many ways it is reached. req.route only exists once the
 * router has matched, which is why the caller reads it at finish and not when
 * the timer starts.
 */
function routeLabel(req) {
  if (!req.route) return 'unmatched';
  return `${req.baseUrl || ''}${req.route.path}`;
}

/**
 * Build the Express app. Dependencies are injected so tests can run without
 * a database or a real metrics registry.
 */
function createApp({ config, logger, metrics, db, redis = null }) {
  const app = express();
  const startedAt = Date.now();

  // Identity of the replica answering this request. os.hostname() is the pod
  // name; the rest comes from the downward API when running in Kubernetes.
  const identity = () => ({
    pod: (config.pod && config.pod.name) || os.hostname(),
    node: (config.pod && config.pod.node) || null,
    ip: (config.pod && config.pod.ip) || null,
    namespace: (config.pod && config.pod.namespace) || null,
  });
  const limits = config.limits;
  app.disable('x-powered-by');
  // Trust exactly one hop: the ingress controller in front of this pod.
  //
  // `true` trusts the whole chain, which means req.ip is the leftmost entry of
  // X-Forwarded-For — a header the client writes. Nothing read req.ip when that
  // was set, so it cost nothing; the moment a limit is keyed on the address it
  // would be a limit the client picks the key for, which is not a limit. One hop
  // makes req.ip the entry ingress-nginx appended, which is the address it
  // actually accepted the connection from.
  //
  // If another proxy is put in front of this, that number goes up. It is the
  // count of proxies you control, not a level of caution: too high trusts a
  // forged hop, too low rate-limits the whole cluster as one client.
  app.set('trust proxy', config.trustedProxyHops);
  app.use(express.json({ limit: '32kb' }));
  app.use(visitorMiddleware(limits));

  const writeLimiter = createRateLimiter({ limit: limits.writesPerMinute });
  const readLimiter = createRateLimiter({ limit: limits.readsPerMinute });

  /**
   * Rate limiting, shared across replicas when Redis is reachable and
   * per-replica when it is not. The fallback is looser, never absent: a shared
   * store being down must not turn into an outage.
   */
  function limited(limiter, limit) {
    return async (req, res, next) => {
      const isWrite = req.method !== 'GET';

      // Keyed on the visitor rather than the IP, because shared networks and
      // mobile carriers put many people behind one address — but only for a
      // visitor who brought an identity with them. One the server minted a
      // moment ago is not a subject to meter: sending no cookie means a fresh
      // key every request, and 60 cookieless posts in one second were measured
      // producing 60 notes with neither the 40-a-minute limit nor the 20-note
      // quota firing once. The one thing such a request does not get to choose
      // is where it came from, so that is what it is metered on.
      //
      // Writes only. A cookieless read is cheap and a strict address limit on
      // reads would break exactly the shared networks the visitor key exists to
      // protect — and the first read is what hands out the cookie that makes
      // the following writes meterable.
      const meterByAddress = isWrite && req.visitorIsNew === true;
      const subject = meterByAddress ? `ip:${req.ip}` : `v:${req.visitorId}`;
      const effective = meterByAddress ? limits.cookielessWritesPerMinute : limit;

      const key = `rl:${isWrite ? 'w' : 'r'}:${subject}`;
      let backend = 'memory';
      let allowed;
      let remaining;
      let retryAfter = 60;

      const shared = redis ? await redis.slidingWindow(key, 60_000) : null;
      if (shared) {
        backend = 'redis';
        allowed = shared.count <= effective;
        remaining = Math.max(0, effective - shared.count);
      } else {
        // The same key as the shared store, not just the visitor id. They had
        // drifted: Redis counted reads and writes in separate buckets and the
        // in-memory fallback counted them in one, so which limit applied
        // depended on whether Redis happened to be up.
        const local = limiter.check(key);
        allowed = local.allowed && local.count <= effective;
        remaining = Math.max(0, effective - local.count);
        retryAfter = local.retryAfter;
      }

      res.setHeader('RateLimit-Remaining', String(remaining));
      res.setHeader('RateLimit-Policy', `${limit};w=60`);
      metrics.limiterDecisions.inc({ backend, outcome: allowed ? 'allowed' : 'limited' });

      if (!allowed) {
        res.setHeader('Retry-After', String(retryAfter));
        // routeLabel here too: this counter carries a route label and a
        // 429 is exactly what a flood of invented paths produces, so the
        // raw path would make the rate limiter its own cardinality bomb.
        metrics.rateLimited.inc({ route: routeLabel(req) });
        return res.status(429).json({ error: 'too many requests, slow down' });
      }
      next();
    };
  }

  const statsCacheKey = (visitor) => `notes:count:${visitor}`;
  // Do not assume config.redis exists just because a client was injected —
  // that coupling is invisible until something rewires the two.
  const statsCacheSeconds = (config.redis && config.redis.statsCacheSeconds) || 3;

  async function invalidateCount(visitor) {
    if (redis) await redis.del(statsCacheKey(visitor));
  }

  // Observe every request except the scrape endpoint itself.
  //
  // What gets logged is deliberately not "every request". A line per healthy
  // request is noise that costs money to store and hides the lines that
  // matter; a service that logs nothing gives an operator nothing to correlate
  // a metric spike against. The middle ground is to log what someone would act
  // on: failures, and requests slow enough to be worth a look.
  app.use((req, res, next) => {
    if (req.path === '/metrics') return next();
    const startedNs = process.hrtime.bigint();
    // No route label yet — see routeLabel. The timer takes the rest of its
    // labels now and the route when it stops.
    const stop = metrics.httpDuration.startTimer({ method: req.method });
    res.on('finish', () => {
      const route = routeLabel(req);
      stop({ route });
      metrics.httpRequests.inc({
        route,
        method: req.method,
        status: res.statusCode,
      });

      const durationMs = Number(process.hrtime.bigint() - startedNs) / 1e6;
      const fields = {
        method: req.method,
        path: req.path,
        status: res.statusCode,
        durationMs: Math.round(durationMs * 100) / 100,
      };

      if (res.statusCode >= 500) logger.error('request failed', fields);
      else if (res.statusCode >= 400) logger.warn('request rejected', fields);
      else if (durationMs >= config.slowRequestMs) logger.warn('slow request', fields);
      else logger.debug('request', fields);
    });
    next();
  });

  app.get('/', (req, res) => {
    res.type('text/plain').send(
      `${config.greeting}! Served by ${os.hostname()} (env: ${config.env})\n`,
    );
  });

  app.get('/config', (req, res) => {
    // Never expose credentials, and never expose internal addresses either.
    // The database hostname is not a secret, but it is a free piece of the
    // cluster's map for anyone probing from outside, and nothing needs it —
    // callers read /stats, not this.
    res.json({
      env: config.env,
      logLevel: config.logLevel,
      databaseConfigured: Boolean(db),
      demoEndpoints: config.demoEndpoints,
    });
  });

  app.get('/notes', limited(readLimiter, limits.readsPerMinute), async (req, res) => {
    if (!db) return res.status(503).json({ error: 'database not configured' });
    try {
      const notes = await db.listNotes(req.visitorId);
      res.json({ servedBy: identity(), count: notes.length, notes });
    } catch (err) {
      logger.error('failed to list notes', { error: err.message });
      res.status(500).json({ error: 'internal error' });
    }
  });

  app.post('/notes', limited(writeLimiter, limits.writesPerMinute), async (req, res) => {
    if (!db) return res.status(503).json({ error: 'database not configured' });
    const raw = req.body && typeof req.body.text === 'string' ? req.body.text : '';
    // Strip control characters; keep tabs and newlines out of stored text.
    const text = raw.replace(/[\u0000-\u001f\u007f]/g, ' ').trim();
    if (!text) return res.status(400).json({ error: 'field "text" is required' });
    if (text.length > limits.maxNoteLength) {
      return res.status(400).json({ error: `field "text" must be ${limits.maxNoteLength} characters or fewer` });
    }
    try {
      const held = await db.countNotesFor(req.visitorId);
      if (held >= limits.maxNotesPerVisitor) {
        return res.status(409).json({
          error: `you can keep ${limits.maxNotesPerVisitor} notes at a time — delete one first`,
        });
      }
      const note = await db.createNote(req.visitorId, text);
      await invalidateCount(req.visitorId);
      res.status(201).json({ servedBy: identity(), note });
    } catch (err) {
      logger.error('failed to create note', { error: err.message });
      res.status(500).json({ error: 'internal error' });
    }
  });

  app.delete('/notes/:id', limited(writeLimiter, limits.writesPerMinute), async (req, res) => {
    if (!db) return res.status(503).json({ error: 'database not configured' });
    const id = Number(req.params.id);
    if (!Number.isInteger(id) || id < 1) {
      return res.status(400).json({ error: 'id must be a positive integer' });
    }
    try {
      const deleted = await db.deleteNote(req.visitorId, id);
      if (!deleted) return res.status(404).json({ error: 'note not found' });
      await invalidateCount(req.visitorId);
      res.json({ servedBy: identity(), deleted });
    } catch (err) {
      logger.error('failed to delete note', { error: err.message });
      res.status(500).json({ error: 'internal error' });
    }
  });

  // Curated read-only summary for callers. The raw /metrics endpoint stays
  // internal — Prometheus scrapes it, the public ingress does not route to it.
  app.get('/stats', limited(readLimiter, limits.readsPerMinute), async (req, res) => {
    let notes = null;
    let databaseUp = false;
    let cached = false;
    if (db) {
      try {
        // /stats is the polled endpoint: callers ask for it repeatedly and the
        // rate-limit checks fire it in bursts, so the count is read far more
        // often than it changes.
        // Short TTL, invalidated on every write, scoped to the caller —
        // /stats must not leak how much other people have stored.
        const key = statsCacheKey(req.visitorId);
        const hit = redis ? await redis.get(key) : null;
        if (hit !== null) {
          notes = Number(hit);
          cached = true;
        } else {
          notes = await db.countNotesFor(req.visitorId);
          if (redis) await redis.setEx(key, statsCacheSeconds, notes);
        }
        databaseUp = true;
      } catch {
        databaseUp = false;
      }
    }
    res.json({
      servedBy: identity(),
      environment: config.env,
      uptimeSeconds: Math.round((Date.now() - startedAt) / 1000),
      notes,
      noteLimit: limits.maxNotesPerVisitor,
      databaseUp,
      cached,
      sharedStore: redis ? redis.isHealthy() : null,
      nodeVersion: process.version,
      memoryMb: Math.round(process.memoryUsage().heapUsed / 1048576),
    });
  });

  // Liveness: process-only. It must NOT depend on the database, otherwise a
  // slow database would restart every pod and turn one outage into two.
  app.get('/healthz', (req, res) => res.type('text/plain').send('ok\n'));

  // Readiness: includes dependencies. A failing check removes the pod from the
  // Service endpoints without killing it.
  app.get('/readyz', async (req, res) => {
    if (!db) return res.type('text/plain').send('ok (no database)\n');
    try {
      await db.ping();
      metrics.dbUp.set(1);
      res.type('text/plain').send('ok\n');
    } catch (err) {
      metrics.dbUp.set(0);
      res.status(503).type('text/plain').send(`database unreachable: ${err.message}\n`);
    }
  });

  // Load generator used to reproduce the documented HPA measurements.
  // Disabled by default so it cannot be abused to burn CPU.
  if (config.demoEndpoints) {
    app.get('/burn', (req, res) => {
      const ms = Math.min(Number(req.query.ms) || 200, 2000);
      const until = Date.now() + ms;
      let sink = 0;
      while (Date.now() < until) sink += Math.sqrt(Math.random());
      res.type('text/plain').send(`burned ${ms}ms of cpu on ${os.hostname()} (${sink > 0})\n`);
    });
    logger.warn('demo endpoints are enabled', { endpoint: '/burn' });
  }

  app.use((req, res) => res.status(404).json({ error: 'not found' }));

  // eslint-disable-next-line no-unused-vars
  app.use((err, req, res, next) => {
    logger.error('unhandled error', { error: err.message });
    res.status(500).json({ error: 'internal error' });
  });

  return app;
}

/**
 * Separate app for the metrics port. Prometheus scrapes this; the ingress does
 * not route to it, so the scrape endpoint is never publicly reachable.
 */
function createMetricsApp({ metrics, db }) {
  const app = express();
  app.disable('x-powered-by');

  app.get('/metrics', async (req, res) => {
    if (db) {
      try {
        metrics.notesTotal.set(await db.countNotes());
      } catch {
        /* keep the previous value; /readyz reports the failure */
      }
    }
    res.set('Content-Type', metrics.registry.contentType);
    res.end(await metrics.registry.metrics());
  });

  app.use((req, res) => res.status(404).end());
  return app;
}

module.exports = { createApp, createMetricsApp };
