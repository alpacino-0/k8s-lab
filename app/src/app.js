'use strict';

const express = require('express');
const os = require('os');

/**
 * Build the Express app. Dependencies are injected so tests can run without
 * a database or a real metrics registry.
 */
function createApp({ config, logger, metrics, db }) {
  const app = express();
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
      res.json({ servedBy: os.hostname(), count: notes.length, notes });
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
      res.status(201).json({ servedBy: os.hostname(), note });
    } catch (err) {
      logger.error('failed to create note', { error: err.message });
      res.status(500).json({ error: 'internal error' });
    }
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

  app.get('/metrics', async (req, res) => {
    if (db) {
      try {
        metrics.notesTotal.set(await db.countNotes());
      } catch {
        /* leave the previous value; /readyz reports the failure */
      }
    }
    res.set('Content-Type', metrics.registry.contentType);
    res.end(await metrics.registry.metrics());
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

module.exports = { createApp };
