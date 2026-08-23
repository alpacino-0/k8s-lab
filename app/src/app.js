'use strict';

const express = require('express');
const os = require('os');
const { visitorMiddleware } = require('./visitor');
const { createRateLimiter } = require('./ratelimit');

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
  // Trust the ingress controller's X-Forwarded-For so proxy hops do not hide
  // the real client from anything that inspects it.
  app.set('trust proxy', true);
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
      // Keyed on the visitor rather than the IP: shared networks and mobile
      // carriers put many people behind a single address.
      const key = `rl:${req.method === 'GET' ? 'r' : 'w'}:${req.visitorId}`;
      let backend = 'memory';
      let allowed;
      let remaining;
      let retryAfter = 60;

      const shared = redis ? await redis.slidingWindow(key, 60_000) : null;
      if (shared) {
        backend = 'redis';
        allowed = shared.count <= limit;
        remaining = Math.max(0, limit - shared.count);
      } else {
        const local = limiter.check(req.visitorId);
        allowed = local.allowed;
        remaining = local.remaining;
        retryAfter = local.retryAfter;
      }

      res.setHeader('RateLimit-Remaining', String(remaining));
      res.setHeader('RateLimit-Policy', `${limit};w=60`);
      metrics.limiterDecisions.inc({ backend, outcome: allowed ? 'allowed' : 'limited' });

      if (!allowed) {
        res.setHeader('Retry-After', String(retryAfter));
        metrics.rateLimited.inc({ route: req.path });
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
    const stop = metrics.httpDuration.startTimer({
      route: req.path,
      method: req.method,
    });
    res.on('finish', () => {
      stop();
      metrics.httpRequests.inc({
        route: req.path,
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
    // the interface reads /stats, not this.
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

  // Curated read-only summary for the UI. The raw /metrics endpoint stays
  // internal — Prometheus scrapes it, the public ingress does not route to it.
  app.get('/stats', limited(readLimiter, limits.readsPerMinute), async (req, res) => {
    let notes = null;
    let databaseUp = false;
    let cached = false;
    if (db) {
      try {
        // The interface polls this every few seconds and the burst button hits
        // it sixty times, so the count is read far more often than it changes.
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
