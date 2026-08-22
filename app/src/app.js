'use strict';

const express = require('express');
const os = require('os');

/**
 * Build the Express app. Dependencies are injected so tests can run without
 * a database or a real metrics registry.
 */
function createApp({ config, logger, metrics, db }) {
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
  app.disable('x-powered-by');
  app.use(express.json({ limit: '64kb' }));

  // Observe every request except the scrape endpoint itself.
  app.use((req, res, next) => {
    if (req.path === '/metrics') return next();
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
      logger.debug('request', {
        method: req.method,
        path: req.path,
        status: res.statusCode,
      });
    });
    next();
  });

  app.get('/', (req, res) => {
    res.type('text/plain').send(
      `${config.greeting}! Served by ${os.hostname()} (env: ${config.env})\n`,
    );
  });

  app.get('/config', (req, res) => {
    // Never expose credentials — only whether they are present.
    res.json({
      env: config.env,
      logLevel: config.logLevel,
      databaseConfigured: Boolean(db),
      databaseHost: config.db ? config.db.host : null,
      demoEndpoints: config.demoEndpoints,
    });
  });

  app.get('/notes', async (req, res) => {
    if (!db) return res.status(503).json({ error: 'database not configured' });
    try {
      const notes = await db.listNotes();
      res.json({ servedBy: identity(), count: notes.length, notes });
    } catch (err) {
      logger.error('failed to list notes', { error: err.message });
      res.status(500).json({ error: 'internal error' });
    }
  });

  app.post('/notes', async (req, res) => {
    if (!db) return res.status(503).json({ error: 'database not configured' });
    const text = req.body && typeof req.body.text === 'string' ? req.body.text.trim() : '';
    if (!text) return res.status(400).json({ error: 'field "text" is required' });
    if (text.length > 500) return res.status(400).json({ error: 'field "text" is too long' });
    try {
      const note = await db.createNote(text);
      res.status(201).json({ servedBy: identity(), note });
    } catch (err) {
      logger.error('failed to create note', { error: err.message });
      res.status(500).json({ error: 'internal error' });
    }
  });

  app.delete('/notes/:id', async (req, res) => {
    if (!db) return res.status(503).json({ error: 'database not configured' });
    const id = Number(req.params.id);
    if (!Number.isInteger(id) || id < 1) {
      return res.status(400).json({ error: 'id must be a positive integer' });
    }
    try {
      const deleted = await db.deleteNote(id);
      if (!deleted) return res.status(404).json({ error: 'note not found' });
      res.json({ servedBy: identity(), deleted });
    } catch (err) {
      logger.error('failed to delete note', { error: err.message });
      res.status(500).json({ error: 'internal error' });
    }
  });

  // Curated read-only summary for the UI. The raw /metrics endpoint stays
  // internal — Prometheus scrapes it, the public ingress does not route to it.
  app.get('/stats', async (req, res) => {
    let notes = null;
    let databaseUp = false;
    if (db) {
      try {
        notes = await db.countNotes();
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
      databaseUp,
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
