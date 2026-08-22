'use strict';

const { loadConfig } = require('./config');
const { createLogger } = require('./logger');
const { createMetrics } = require('./metrics');
const { createDb } = require('./db');
const { createApp, createMetricsApp } = require('./app');

const config = loadConfig();
const logger = createLogger({ level: config.logLevel, base: { env: config.env } });
const metrics = createMetrics();
const db = createDb(config.db, logger);
const app = createApp({ config, logger, metrics, db });
const metricsApp = createMetricsApp({ metrics, db });

const server = app.listen(config.port, () => {
  logger.info('listening', {
    port: config.port,
    database: config.db ? config.db.host : null,
  });
});

const metricsServer = metricsApp.listen(config.metricsPort, () => {
  logger.info('metrics listening', { port: config.metricsPort });
});

let shuttingDown = false;

/**
 * Graceful shutdown. As PID 1 in a container, a process that installs no
 * signal handler ignores SIGTERM entirely: Kubernetes then waits for
 * terminationGracePeriodSeconds and SIGKILLs it, adding ~30s to every rollout
 * and cutting in-flight requests.
 */
async function shutdown(signal) {
  if (shuttingDown) return;
  shuttingDown = true;
  logger.info('shutting down', { signal });

  const force = setTimeout(() => {
    logger.error('graceful shutdown timed out, forcing exit');
    process.exit(1);
  }, config.shutdownTimeoutMs);
  force.unref();

  metricsServer.close();
  server.close(async () => {
    try {
      if (db) await db.close();
      logger.info('shutdown complete');
      process.exit(0);
    } catch (err) {
      logger.error('error during shutdown', { error: err.message });
      process.exit(1);
    }
  });
}

process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT', () => shutdown('SIGINT'));

process.on('unhandledRejection', (reason) => {
  logger.error('unhandled promise rejection', { error: String(reason) });
});
process.on('uncaughtException', (err) => {
  logger.error('uncaught exception, exiting', { error: err.message, stack: err.stack });
  process.exit(1);
});
