'use strict';

/** Parse and validate configuration from the environment. Fail fast on bad input. */
function loadConfig(env = process.env) {
  const num = (value, fallback, name) => {
    if (value === undefined || value === '') return fallback;
    const parsed = Number(value);
    if (!Number.isFinite(parsed) || parsed <= 0) {
      throw new Error(`${name} must be a positive number, got: ${value}`);
    }
    return parsed;
  };

  return {
    env: env.APP_ENV || 'development',
    logLevel: env.LOG_LEVEL || 'info',
    greeting: env.GREETING || 'Hello',
    port: num(env.PORT, 3000, 'PORT'),
    // Telemetry listens on its own port so the public ingress, which only
    // routes to the application port, cannot reach the scrape endpoint.
    metricsPort: num(env.METRICS_PORT, 9090, 'METRICS_PORT'),
    shutdownTimeoutMs: num(env.SHUTDOWN_TIMEOUT_MS, 10000, 'SHUTDOWN_TIMEOUT_MS'),
    demoEndpoints: env.DEMO_ENDPOINTS === 'true',
    // Above this, a successful request is still worth a line.
    slowRequestMs: num(env.SLOW_REQUEST_MS, 250, 'SLOW_REQUEST_MS'),
    // How many proxies in front of this process are ours. Express reads
    // req.ip by counting back this many entries of X-Forwarded-For, so
    // this is what stops a client from choosing its own rate-limit key by
    // writing the header. One is the ingress controller.
    trustedProxyHops: num(env.TRUSTED_PROXY_HOPS, 1, 'TRUSTED_PROXY_HOPS'),

    // Public-exposure limits. Anyone can use the demo without signing up, so
    // the guard rails are per visitor rather than per account.
    limits: {
      // Notes are scoped to an anonymous visitor cookie: no signup friction,
      // but nobody reads or deletes anyone else's notes.
      visitorCookie: env.VISITOR_COOKIE || 'visitor',
      visitorCookieDays: num(env.VISITOR_COOKIE_DAYS, 30, 'VISITOR_COOKIE_DAYS'),
      secureCookie: env.SECURE_COOKIE === 'true',
      maxNotesPerVisitor: num(env.MAX_NOTES_PER_VISITOR, 20, 'MAX_NOTES_PER_VISITOR'),
      maxNoteLength: num(env.MAX_NOTE_LENGTH, 500, 'MAX_NOTE_LENGTH'),
      // Backstop only. Each replica counts on its own, so the effective limit
      // is this multiplied by the replica count — the ingress limiter is the
      // one that actually bounds a client.
      writesPerMinute: num(env.WRITES_PER_MINUTE, 40, 'WRITES_PER_MINUTE'),
      // The floor for a write from a visitor who presented no cookie.
      //
      // Every other limit here is per visitor, and a visitor with no
      // cookie is a brand-new one every request — measured: 60 cookieless
      // posts in one second produced 60 notes, no rate limit and no quota,
      // against limits of 40 a minute and 20 notes. So a write that
      // arrives with no identity is metered on the address instead, which
      // is the one thing about such a request the client does not choose.
      // Low on purpose: a real visitor pays it once and then has a cookie.
      cookielessWritesPerMinute: num(
        env.COOKIELESS_WRITES_PER_MINUTE, 5, 'COOKIELESS_WRITES_PER_MINUTE'),
      readsPerMinute: num(env.READS_PER_MINUTE, 240, 'READS_PER_MINUTE'),
    },

    // Shared counters and a small read cache. Optional: without it the service
    // runs exactly as before, with per-replica limits and no caching.
    redis: env.REDIS_URL
      ? {
          url: env.REDIS_URL,
          connectTimeoutMs: num(env.REDIS_CONNECT_TIMEOUT_MS, 2000, 'REDIS_CONNECT_TIMEOUT_MS'),
          statsCacheSeconds: num(env.STATS_CACHE_SECONDS, 3, 'STATS_CACHE_SECONDS'),
        }
      : null,
    // Filled by the Kubernetes downward API. Reading these needs no API access
    // and no service-account token — the kubelet injects them into the pod.
    pod: {
      name: env.POD_NAME || null,
      namespace: env.POD_NAMESPACE || null,
      ip: env.POD_IP || null,
      node: env.NODE_NAME || null,
    },
    db: env.DB_HOST
      ? {
          host: env.DB_HOST,
          port: num(env.DB_PORT, 5432, 'DB_PORT'),
          database: env.POSTGRES_DB || 'labdb',
          user: env.POSTGRES_USER || 'labuser',
          password: env.POSTGRES_PASSWORD || '',
          poolMax: num(env.DB_POOL_MAX, 5, 'DB_POOL_MAX'),
          connectionTimeoutMillis: num(env.DB_CONNECT_TIMEOUT_MS, 3000, 'DB_CONNECT_TIMEOUT_MS'),
          idleTimeoutMillis: num(env.DB_IDLE_TIMEOUT_MS, 30000, 'DB_IDLE_TIMEOUT_MS'),
        }
      : null,
  };
}

module.exports = { loadConfig };
