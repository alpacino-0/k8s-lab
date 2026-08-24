'use strict';

const client = require('prom-client');

function createMetrics({ appName = 'damga-app' } = {}) {
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

  const cacheRequests = new client.Counter({
    name: 'cache_requests_total',
    help: 'Cache lookups by outcome',
    labelNames: ['result'],
    registers: [registry],
  });

  const redisUp = new client.Gauge({
    name: 'redis_up',
    help: '1 when the shared store is reachable, 0 when the service is running on its fallback',
    registers: [registry],
  });

  const limiterDecisions = new client.Counter({
    name: 'rate_limiter_decisions_total',
    help: 'Rate limit decisions by backend and outcome',
    labelNames: ['backend', 'outcome'],
    registers: [registry],
  });

  const dbUp = new client.Gauge({
    name: 'database_up',
    help: '1 when the last database health check succeeded, 0 otherwise',
    registers: [registry],
  });

  return {
    registry,
    httpRequests,
    httpDuration,
    notesTotal,
    dbUp,
    rateLimited,
    cacheRequests,
    redisUp,
    limiterDecisions,
  };
}

module.exports = { createMetrics };
