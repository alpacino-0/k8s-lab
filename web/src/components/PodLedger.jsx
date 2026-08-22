import { shortPod } from '../api';

export function PodLedger({ lanes, summary, onProbe, onReset, probing }) {
  return (
    <section className="ledger" aria-labelledby="ledger-title">
      <header className="ledger-head">
        <div>
          <p className="eyebrow" id="ledger-title">
            Pod ledger — one lane per replica that has answered you
          </p>
        </div>
        <div style={{ display: 'flex', gap: '0.5rem' }}>
          <button className="btn" onClick={onProbe} disabled={probing}>
            {probing ? 'Sending…' : 'Send 60 requests'}
          </button>
          <button className="btn btn-quiet" onClick={onReset} disabled={probing || !lanes.length}>
            Clear
          </button>
        </div>
      </header>

      <div className="ledger-lanes">
        {lanes.length === 0 ? (
          <p className="ledger-empty">
            No requests recorded yet. Add a note, or send a burst — each response
            drops a mark into the lane of the replica that produced it.
          </p>
        ) : (
          lanes.map((lane) => {
            const share = summary.total ? Math.round((lane.count / summary.total) * 100) : 0;
            return (
              <div className="lane" key={lane.pod} style={{ '--lane-pen': lane.pen }}>
                <div className="lane-id">
                  <b title={lane.pod}>{shortPod(lane.pod)}</b>
                  {lane.node && <span className="lane-node">{lane.node}</span>}
                </div>
                <div
                  className="track"
                  role="img"
                  aria-label={`${lane.count} requests served by ${lane.pod}`}
                >
                  {lane.ticks.map((tick, i) => (
                    <i className="tick" key={`${tick}-${i}`} />
                  ))}
                </div>
                <div className="lane-count">
                  {lane.count} <span>{share}%</span>
                </div>
              </div>
            );
          })
        )}
      </div>
    </section>
  );
}
