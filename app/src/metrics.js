'use strict';

const client = require('prom-client');

function createMetrics({ appName = 'k8s-lab-app' } = {}) {
  const registry = new client.Registry();
  registry.setDefaultLabels({ app: appName });
  client.collectDefaultMetrics({ register: registry });

  const httpRequests = new client.Counter({
    name: 'http_requests_total',
    help: 'Total HTTP requests',
    labelNames: ['route', 'method', 'status'],
    registers: [registry],
  });

  const httpDuration = new client.Histogram({
    name: 'http_request_duration_seconds',
    help: 'HTTP request duration in seconds',
    labelNames: ['route', 'method'],
    buckets: [0.005, 0.01, 0.05, 0.1, 0.3, 0.5, 1, 2, 5],
    registers: [registry],
  });

  const notesTotal = new client.Gauge({
    name: 'notes_total',
    help: 'Number of notes stored in the database',
    registers: [registry],
  });

  const rateLimited = new client.Counter({
    name: 'rate_limited_total',
    help: 'Requests rejected by the in-process rate limiter',
    labelNames: ['route'],
    registers: [registry],
  });

  const dbUp = new client.Gauge({
    name: 'database_up',
    help: '1 when the last database health check succeeded, 0 otherwise',
    registers: [registry],
  });

  return { registry, httpRequests, httpDuration, notesTotal, dbUp, rateLimited };
}

module.exports = { createMetrics };
