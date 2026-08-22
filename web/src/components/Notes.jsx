import { useState } from 'react';
import { shortPod } from '../api';

function when(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString();
}

export function Notes({ notes, loading, onCreate, onDelete, penFor }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);

  async function submit(event) {
    event.preventDefault();
    const value = text.trim();
    if (!value || busy) return;
    setBusy(true);
    try {
      await onCreate(value);
      setText('');
    } finally {
      setBusy(false);
    }
  }

  return (
    <section aria-labelledby="notes-title">
      <p className="eyebrow">The application</p>
      <h2 className="panel-title" id="notes-title">
        Notes
      </h2>

      <form className="composer" onSubmit={submit}>
        <input
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="Write a note"
          maxLength={500}
          aria-label="Note text"
        />
        <button className="btn" type="submit" disabled={busy || !text.trim()}>
          Add note
        </button>
      </form>

      {loading && notes.length === 0 ? (
        <p className="empty">Loading notes…</p>
      ) : notes.length === 0 ? (
        <p className="empty">
          Nothing here yet. Add the first note — the reply will tell you which
          replica handled it, and the row below records it.
        </p>
      ) : (
        <ul className="note-list">
          {notes.map((note) => (
            <li className="note" key={note.id} style={{ '--lane-pen': penFor(note.servedByPod) }}>
              <p className="note-text">{note.text}</p>
              <div className="note-meta">
                <span>#{note.id}</span>
                <span>{when(note.created_at)}</span>
                {note.servedByPod && (
                  <span className="pen">handled by {shortPod(note.servedByPod)}</span>
                )}
                <button
                  className="btn-tiny"
                  onClick={() => onDelete(note.id)}
                  aria-label={`Delete note ${note.id}`}
                >
                  Delete
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
