'use strict';

/**
 * Fixed-window counter, per key, in memory.
 *
 * Deliberately simple and deliberately a backstop: each replica keeps its own
 * counters, so a client spread across N replicas gets N times the allowance.
 * The ingress controller enforces the real per-IP limit. This exists so a
 * single pod cannot be hammered into the database even if that layer is
 * misconfigured or bypassed inside the cluster.
 */
function createRateLimiter({ limit, windowMs = 60_000, now = () => Date.now() }) {
  const windows = new Map();

  function sweep(current) {
    for (const [key, entry] of windows) {
      if (entry.resetAt <= current) windows.delete(key);
    }
  }

  return {
    /** @returns {{allowed: boolean, remaining: number, retryAfter: number}} */
    check(key) {
      const current = now();
      // Cheap amortised cleanup; the map only holds active windows.
      if (windows.size > 5000) sweep(current);

      let entry = windows.get(key);
      if (!entry || entry.resetAt <= current) {
        entry = { count: 0, resetAt: current + windowMs };
        windows.set(key, entry);
      }
      entry.count += 1;
      const allowed = entry.count <= limit;
      return {
        allowed,
        remaining: Math.max(0, limit - entry.count),
        retryAfter: Math.ceil((entry.resetAt - current) / 1000),
      };
    },
    size: () => windows.size,
  };
}

module.exports = { createRateLimiter };
