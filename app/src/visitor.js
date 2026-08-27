'use strict';

const crypto = require('node:crypto');

/** Parse a Cookie header without pulling in a dependency for one field. */
function readCookie(header, name) {
  if (!header) return null;
  for (const part of header.split(';')) {
    const eq = part.indexOf('=');
    if (eq === -1) continue;
    if (part.slice(0, eq).trim() === name) {
      return decodeURIComponent(part.slice(eq + 1).trim());
    }
  }
  return null;
}

const ID_PATTERN = /^[0-9a-f]{32}$/;

/**
 * Gives every browser an anonymous identity so notes can be scoped to it.
 *
 * This is not authentication and does not pretend to be: it stops visitors
 * from reading and deleting each other's notes, and it bounds how much any one
 * of them can store. A public demo that required a login would have no
 * visitors; one with a shared, unbounded table becomes a spam wall within
 * hours.
 */
function visitorMiddleware(limits) {
  return (req, res, next) => {
    const existing = readCookie(req.headers.cookie, limits.visitorCookie);
    const id = existing && ID_PATTERN.test(existing) ? existing : crypto.randomBytes(16).toString('hex');

    if (id !== existing) {
      const maxAge = limits.visitorCookieDays * 24 * 60 * 60;
      const parts = [
        `${limits.visitorCookie}=${id}`,
        'Path=/',
        'HttpOnly',
        'SameSite=Lax',
        `Max-Age=${maxAge}`,
      ];
      if (limits.secureCookie) parts.push('Secure');
      res.setHeader('Set-Cookie', parts.join('; '));
    }

    req.visitorId = id;
    // Whether this identity was minted a line ago or arrived with the request.
    // A caller that meters anything per visitor has to know: an identity the
    // server just invented is not a subject, it is a blank cheque, and handing
    // out a fresh one per request is what let a cookieless loop write past
    // every limit the demo has.
    req.visitorIsNew = id !== existing;
    next();
  };
}

module.exports = { visitorMiddleware, readCookie };
