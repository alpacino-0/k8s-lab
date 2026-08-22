'use strict';

const { Pool } = require('pg');

/**
 * Database gateway. Returns null when no DB is configured so the app can run
 * standalone (useful for the Helm dev profile and for unit tests).
 */
function createDb(dbConfig, logger) {
  if (!dbConfig) return null;

  const pool = new Pool({
    host: dbConfig.host,
    port: dbConfig.port,
    database: dbConfig.database,
    user: dbConfig.user,
    password: dbConfig.password,
    max: dbConfig.poolMax,
    connectionTimeoutMillis: dbConfig.connectionTimeoutMillis,
    idleTimeoutMillis: dbConfig.idleTimeoutMillis,
  });

  // Without this listener an idle-client error is an unhandled 'error' event,
  // which crashes the process. A database restart must not take the app down.
  pool.on('error', (err) => {
    logger.error('database pool error (swallowed)', { error: err.message });
  });

  return {
    async ping() {
      await pool.query('SELECT 1');
    },
    async listNotes(owner, limit = 20) {
      const { rows } = await pool.query(
        'SELECT id, text, created_at FROM notes WHERE owner = $1 ORDER BY id DESC LIMIT $2',
        [owner, limit],
      );
      return rows;
    },
    async createNote(owner, text) {
      const { rows } = await pool.query(
        'INSERT INTO notes (owner, text) VALUES ($1, $2) RETURNING id, text, created_at',
        [owner, text],
      );
      return rows[0];
    },
    async countNotesFor(owner) {
      const { rows } = await pool.query(
        'SELECT count(*)::int AS n FROM notes WHERE owner = $1',
        [owner],
      );
      return rows[0].n;
    },
    async deleteNote(owner, id) {
      // Scoped by owner: a visitor cannot delete a note they cannot see.
      const { rows } = await pool.query(
        'DELETE FROM notes WHERE id = $1 AND owner = $2 RETURNING id, text, created_at',
        [id, owner],
      );
      return rows[0] || null;
    },
    async countNotes() {
      const { rows } = await pool.query('SELECT count(*)::int AS n FROM notes');
      return rows[0].n;
    },
    async close() {
      await pool.end();
    },
  };
}

module.exports = { createDb };
