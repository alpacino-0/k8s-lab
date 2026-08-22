'use strict';

const { createClient } = require('redis');

/**
 * Shared state for things that must be counted across replicas.
 *
 * The in-process limiter counts per pod, so a client spread over N replicas
 * gets N times its allowance. Redis makes the window shared, which is the
 * whole reason it is here.
 *
 * Every method degrades instead of throwing. A cache or a rate limiter that
 * takes the service down when it is unavailable is worse than not having one:
 * the caller falls back to the in-process limiter and the request still gets
 * served.
 */
function createRedis(redisConfig, logger, metrics) {
  if (!redisConfig) return null;

  const client = createClient({
    url: redisConfig.url,
    socket: {
      connectTimeout: redisConfig.connectTimeoutMs,
      // Give up reconnecting exponentially rather than hammering a dead host.
      reconnectStrategy: (retries) => Math.min(200 * 2 ** retries, 10_000),
    },
  });

  let healthy = false;

  client.on('error', (err) => {
    if (healthy) logger.warn('redis unavailable, falling back', { error: err.message });
    healthy = false;
    metrics.redisUp.set(0);
  });
  client.on('ready', () => {
    logger.info('redis ready', { url: redisConfig.url });
    healthy = true;
    metrics.redisUp.set(1);
  });

  client.connect().catch((err) => {
    logger.warn('redis initial connection failed, using in-process fallback', {
      error: err.message,
    });
  });

  return {
    isHealthy: () => healthy,

    /**
     * Sliding-window counter over a sorted set. Older entries are trimmed by
     * score, the new one is added, and the cardinality is the count for the
     * window — no fixed-window edge where twice the limit slips through at a
     * boundary.
     *
     * @returns {Promise<{count: number}|null>} null when Redis is unavailable
     */
    async slidingWindow(key, windowMs, now = Date.now()) {
      if (!healthy) return null;
      try {
        const results = await client
          .multi()
          .zRemRangeByScore(key, 0, now - windowMs)
          .zAdd(key, { score: now, value: `${now}-${Math.random().toString(36).slice(2, 10)}` })
          .zCard(key)
          .pExpire(key, windowMs)
          .exec();
        const count = Number(results[2]);
        return Number.isFinite(count) ? { count } : null;
      } catch (err) {
        logger.warn('redis rate limit failed, falling back', { error: err.message });
        return null;
      }
    },

    async get(key) {
      if (!healthy) return null;
      try {
        const value = await client.get(key);
        metrics.cacheRequests.inc({ result: value === null ? 'miss' : 'hit' });
        return value;
      } catch (err) {
        logger.warn('redis get failed', { error: err.message });
        metrics.cacheRequests.inc({ result: 'error' });
        return null;
      }
    },

    async setEx(key, seconds, value) {
      if (!healthy) return;
      try {
        await client.setEx(key, seconds, String(value));
      } catch (err) {
        logger.warn('redis set failed', { error: err.message });
      }
    },

    async del(key) {
      if (!healthy) return;
      try {
        await client.del(key);
      } catch (err) {
        logger.warn('redis delete failed', { error: err.message });
      }
    },

    async close() {
      try {
        await client.quit();
      } catch {
        /* already gone */
      }
    },
  };
}

module.exports = { createRedis };
