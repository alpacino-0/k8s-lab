import { useCallback, useMemo, useRef, useState } from 'react';

const PENS = [
  'var(--pen-1)',
  'var(--pen-2)',
  'var(--pen-3)',
  'var(--pen-4)',
  'var(--pen-5)',
  'var(--pen-6)',
];

const MAX_TICKS = 90;

/**
 * Records which replica answered each request.
 *
 * This is not read from the Kubernetes API — the page has no credentials for
 * it. Every response carries the identity of the pod that produced it, and the
 * ledger is simply the tally of what came back. Load balancing is demonstrated
 * rather than reported.
 */
export function usePodLedger() {
  const [lanes, setLanes] = useState([]);
  const [last, setLast] = useState(null);
  const penByPod = useRef(new Map());

  const penFor = useCallback((pod) => {
    if (!penByPod.current.has(pod)) {
      penByPod.current.set(pod, PENS[penByPod.current.size % PENS.length]);
    }
    return penByPod.current.get(pod);
  }, []);

  const record = useCallback(
    (servedBy) => {
      if (!servedBy || !servedBy.pod) return;
      const { pod, node } = servedBy;
      setLast({ pod, node, pen: penFor(pod) });
      setLanes((current) => {
        const index = current.findIndex((lane) => lane.pod === pod);
        if (index === -1) {
          return [...current, { pod, node, pen: penFor(pod), count: 1, ticks: [Date.now()] }];
        }
        const next = current.slice();
        const lane = next[index];
        next[index] = {
          ...lane,
          node: node || lane.node,
          count: lane.count + 1,
          ticks: [...lane.ticks, Date.now()].slice(-MAX_TICKS),
        };
        return next;
      });
    },
    [penFor],
  );

  const reset = useCallback(() => {
    setLanes([]);
    setLast(null);
    penByPod.current = new Map();
  }, []);

  const summary = useMemo(() => {
    const total = lanes.reduce((sum, lane) => sum + lane.count, 0);
    const nodes = new Set(lanes.map((lane) => lane.node).filter(Boolean));
    return { total, replicas: lanes.length, nodes: nodes.size };
  }, [lanes]);

  return { lanes, last, summary, record, reset };
}
