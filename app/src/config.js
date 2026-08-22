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
    shutdownTimeoutMs: num(env.SHUTDOWN_TIMEOUT_MS, 10000, 'SHUTDOWN_TIMEOUT_MS'),
    demoEndpoints: env.DEMO_ENDPOINTS === 'true',
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
