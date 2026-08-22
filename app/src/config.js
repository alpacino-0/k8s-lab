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
      readsPerMinute: num(env.READS_PER_MINUTE, 240, 'READS_PER_MINUTE'),
    },
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
