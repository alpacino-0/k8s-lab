'use strict';

const LEVELS = { error: 50, warn: 40, info: 30, debug: 20 };

/**
 * Minimal structured logger. Emits one JSON object per line so log
 * aggregators (Loki, CloudWatch, Datadog) can parse without regexes.
 */
function createLogger({ level = 'info', base = {} } = {}) {
  const threshold = LEVELS[level] ?? LEVELS.info;

  const emit = (lvl, msg, fields = {}) => {
    if (LEVELS[lvl] < threshold) return;
    const line = JSON.stringify({
      ts: new Date().toISOString(),
      level: lvl,
      msg,
      ...base,
      ...fields,
    });
    if (lvl === 'error' || lvl === 'warn') process.stderr.write(line + '\n');
    else process.stdout.write(line + '\n');
  };

  return {
    error: (msg, fields) => emit('error', msg, fields),
    warn: (msg, fields) => emit('warn', msg, fields),
    info: (msg, fields) => emit('info', msg, fields),
    debug: (msg, fields) => emit('debug', msg, fields),
  };
}

module.exports = { createLogger, LEVELS };
