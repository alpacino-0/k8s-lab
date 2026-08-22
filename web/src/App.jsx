import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { api, shortPod } from './api';
import { usePodLedger } from './usePodLedger';
import { PodLedger } from './components/PodLedger';
import { Notes } from './components/Notes';
import { Mechanisms } from './components/Mechanisms';

const REPO = 'https://github.com/alpacino-0/k8s-lab';

export default function App() {
  const { lanes, last, summary, record, reset } = usePodLedger();
  const [notes, setNotes] = useState([]);
  const [stats, setStats] = useState(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [probing, setProbing] = useState(false);
  const penIndex = useRef(new Map());

  // Pen assignment mirrors the ledger so a note and its lane share a colour.
  const penFor = useCallback(
    (pod) => {
      const lane = lanes.find((l) => l.pod === pod);
      if (lane) return lane.pen;
      if (!penIndex.current.has(pod)) penIndex.current.set(pod, 'var(--ink-faint)');
      return penIndex.current.get(pod);
    },
    [lanes],
  );

  const refresh = useCallback(async () => {
    try {
      const body = await api.listNotes();
      record(body.servedBy);
      setNotes(body.notes.map((n) => ({ ...n, servedByPod: body.servedBy?.pod })));
      setError(null);
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  }, [record]);

  const pollStats = useCallback(async () => {
    try {
      const body = await api.stats();
      record(body.servedBy);
      setStats(body);
      setError(null);
    } catch (err) {
      setError(err.message);
    }
  }, [record]);

  useEffect(() => {
    refresh();
    pollStats();
    const timer = setInterval(pollStats, 5000);
    return () => clearInterval(timer);
  }, [refresh, pollStats]);

  async function createNote(text) {
    try {
      const body = await api.createNote(text);
      record(body.servedBy);
      setNotes((current) => [
        { ...body.note, servedByPod: body.servedBy?.pod },
        ...current,
      ]);
      setError(null);
    } catch (err) {
      setError(err.message);
    }
  }

  async function deleteNote(id) {
    try {
      const body = await api.deleteNote(id);
      record(body.servedBy);
      setNotes((current) => current.filter((n) => n.id !== id));
      setError(null);
    } catch (err) {
      setError(err.message);
    }
  }

  // A burst large enough to make the distribution obvious, small enough to stay
  // polite. Sent in waves so the ledger draws rather than snapping into place.
  async function probe() {
    setProbing(true);
    try {
      for (let wave = 0; wave < 6; wave += 1) {
        const results = await Promise.all(
          Array.from({ length: 10 }, () => api.stats().catch(() => null)),
        );
        results.forEach((r) => r && record(r.servedBy));
        await new Promise((resolve) => setTimeout(resolve, 120));
      }
      setError(null);
    } catch (err) {
      setError(err.message);
    } finally {
      setProbing(false);
    }
  }

  const replicaWord = summary.replicas === 1 ? 'replica' : 'replicas';
  const currentPen = last?.pen || 'var(--pen-1)';

  const facts = useMemo(
    () => [
      { term: 'Environment', value: stats?.environment ?? '—' },
      { term: 'Replicas seen', value: summary.replicas || '—' },
      { term: 'Nodes seen', value: summary.nodes || '—' },
      { term: 'Requests recorded', value: summary.total || 0 },
      {
        term: 'Database',
        value: stats ? (stats.databaseUp ? 'reachable' : 'unreachable') : '—',
        state: stats ? (stats.databaseUp ? 'up' : 'down') : undefined,
      },
      { term: 'Notes stored', value: stats?.notes ?? '—' },
      { term: 'Replica uptime', value: stats ? `${stats.uptimeSeconds}s` : '—' },
      { term: 'Heap in use', value: stats ? `${stats.memoryMb} MB` : '—' },
      { term: 'Runtime', value: stats?.nodeVersion ?? '—' },
    ],
    [stats, summary],
  );

  return (
    <>
      <header className="masthead" style={{ '--pen-current': currentPen }}>
        <div className="wordmark">
          k8s-lab <span>notes service</span>
        </div>
        <div className="served">
          <i className="beacon" data-state={error ? 'down' : 'up'} aria-hidden="true" />
          {error ? (
            <span>backend unreachable</span>
          ) : (
            <span>
              this response came from <b>{last ? shortPod(last.pod) : '…'}</b>
              {last?.node ? ` on ${last.node}` : ''}
            </span>
          )}
        </div>
      </header>

      <main className="shell">
        <section className="hero">
          <p className="eyebrow">A working service, and the platform under it</p>
          <h1 className="thesis" style={{ '--pen-current': currentPen }}>
            You are talking to <span className="count">{summary.replicas || '—'}</span> {replicaWord}{' '}
            <em>without choosing one</em>
          </h1>
          <p className="prose">
            This is an ordinary notes application. Everything interesting about it is
            underneath: how many copies are running, which one answered, what happens
            when one dies, and what stops the others from reaching things they should
            not. Use it, and the record below fills in.
          </p>

          {error && (
            <p className="banner" role="status">
              {error} — if the deployment is mid-rollout this clears on its own.
            </p>
          )}

          <PodLedger
            lanes={lanes}
            summary={summary}
            onProbe={probe}
            onReset={reset}
            probing={probing}
          />
        </section>

        <hr className="rule" />

        <div className="columns">
          <Notes
            notes={notes}
            loading={loading}
            onCreate={createNote}
            onDelete={deleteNote}
            penFor={penFor}
          />

          <section aria-labelledby="facts-title">
            <p className="eyebrow">Reported by the replica that answered</p>
            <h2 className="panel-title" id="facts-title">
              Live readings
            </h2>
            <dl className="facts">
              {facts.map((fact) => (
                <div className="fact" key={fact.term}>
                  <dt>{fact.term}</dt>
                  <dd data-state={fact.state}>{fact.value}</dd>
                </div>
              ))}
            </dl>
            <p className="note-hint">
              Refreshed every 5 seconds. &ldquo;Replicas seen&rdquo; counts the distinct pods
              that have answered this browser — not a cluster query. Uptime resets when a
              replica is replaced, which is the quickest way to notice a rollout.
            </p>
          </section>
        </div>

        <hr className="rule" />

        <Mechanisms />

        <footer className="footer">
          <span>
            Source, measurements and the bugs found along the way:{' '}
            <a href={REPO} target="_blank" rel="noreferrer">
              github.com/alpacino-0/k8s-lab
            </a>
          </span>
          <span>MIT</span>
        </footer>
      </main>
    </>
  );
}
