const REPO = 'https://github.com/alpacino-0/k8s-lab';

const MECHANISMS = [
  {
    pen: 'var(--pen-1)',
    eyebrow: 'Traffic',
    title: 'Requests spread across replicas',
    prose: `Three copies of this service run at once. A Service sits in front of them and
      spreads requests across whichever ones are healthy. Nothing in the page picks a
      replica — the ledger above is just a tally of who answered, which is why the
      distribution is never perfectly even.`,
    evidence: [
      { label: 'Measured', figure: '30 requests → 12 / 11 / 7' },
      { label: 'Live', text: 'The lanes above are recording this session.' },
    ],
    where: 'chart/templates/service.yaml',
  },
  {
    pen: 'var(--pen-2)',
    eyebrow: 'Health checks',
    title: 'Two questions, not one',
    prose: `Kubernetes asks two separate things: can this replica take traffic, and is it
      still alive. They get different answers here. Readiness checks the database;
      liveness only checks that the process responds. If liveness checked the database
      too, one slow database would restart every replica at once and turn a single
      outage into two.`,
    evidence: [
      { label: 'Test', code: 'kubectl scale statefulset postgres --replicas=0' },
      { label: 'Result', figure: '0 restarts' },
      { label: 'Recovery', text: 'Ready again 8s after the database returned.' },
    ],
    where: 'app/src/app.js — /healthz and /readyz',
  },
  {
    pen: 'var(--pen-3)',
    eyebrow: 'Releases',
    title: 'Deploys that drop nothing',
    prose: `New replicas have to pass their readiness check before any old one is removed,
      and the rollout is configured to never dip below the desired count. During a
      release both versions serve traffic at the same time, which is a constraint on
      database changes as much as a feature.`,
    evidence: [
      { label: 'Measured, every push', figure: '200 requests → 0 failed' },
      { label: 'Where', text: 'Enforced by the CI pipeline, not by hand.' },
    ],
    where: '.github/workflows/ci.yml — "Upgrade must be zero-downtime"',
  },
  {
    pen: 'var(--pen-4)',
    eyebrow: 'Maintenance',
    title: 'Draining a machine, unnoticed',
    prose: `Taking a node out of service evicts its pods. Removing a pod from the load
      balancer and stopping its process happen at the same moment, so a request can
      arrive at a process that is already shutting down. A short delay before shutdown
      closes that gap.`,
    evidence: [
      { label: 'Without the delay', figure: '2 of 200 dropped' },
      { label: 'With it', figure: '0 of 300 dropped' },
    ],
    where: 'chart/values.yaml — lifecycle.preStopSleepSeconds',
  },
  {
    pen: 'var(--pen-5)',
    eyebrow: 'Containment',
    title: 'A container that can barely do anything',
    prose: `This service runs as an unprivileged user on a filesystem it cannot write to,
      with every Linux capability dropped and privilege escalation blocked. If someone
      found a way to run code in it, there is very little for them to do next and
      nowhere to put a file.`,
    evidence: [
      { label: 'Asserted on the running pod', text: 'non-root · read-only rootfs · capabilities: drop ALL' },
      { label: 'Image scan', figure: '0 critical, 0 high' },
    ],
    where: 'chart/values.yaml — containerSecurityContext',
  },
  {
    pen: 'var(--pen-6)',
    eyebrow: 'Least privilege',
    title: 'This page holds no cluster credentials',
    prose: `Reading the replica list from the Kubernetes API would have been the easy way
      to build the ledger — and it would have meant mounting a credential into a pod
      that has no other reason to hold one. Instead each response carries the identity
      of the pod that produced it, taken from environment variables the kubelet injects.
      The ledger counts what came back. Same picture, nothing to steal.`,
    evidence: [
      { label: 'Service account token', text: 'Not mounted (automountServiceAccountToken: false)' },
      { label: 'Source of identity', code: 'POD_NAME, NODE_NAME — downward API' },
    ],
    where: 'chart/templates/deployment.yaml',
  },
  {
    pen: 'var(--pen-1)',
    eyebrow: 'Network',
    title: 'The database answers almost nobody',
    prose: `By default any pod in a cluster can open a connection to any other. Here the
      database accepts traffic only from this service and its migration job, and the
      service itself accepts traffic only from the ingress controller and the metrics
      scraper. The rule that is easiest to forget is the one allowing DNS — without it
      a locked-down pod cannot resolve anything and every call times out for no visible
      reason.`,
    evidence: [
      { label: 'Before', text: 'An unrelated pod read the whole notes table.' },
      { label: 'After', code: 'psql: connection timeout expired' },
      { label: 'Checked on every push', text: 'An unauthorized pod must fail to connect.' },
    ],
    where: 'chart/templates/networkpolicy.yaml',
  },
  {
    pen: 'var(--pen-2)',
    eyebrow: 'Monitoring',
    title: 'An alert that fires when everything vanishes',
    prose: `The obvious way to alert on a dead service is to check that its health metric
      equals zero. That rule stays silent in the worst case: when every replica
      disappears the metric stops existing, and a comparison against nothing is never
      true. Both rules were run side by side under the same outage.`,
    evidence: [
      { label: 'up == 0', text: 'inactive — never fired' },
      { label: 'absent(up{…})', figure: 'firing' },
    ],
    where: 'chart/templates/prometheusrule.yaml',
  },
];

export function Mechanisms() {
  return (
    <section className="mechanisms" aria-labelledby="mech-title">
      <p className="eyebrow">What is running underneath</p>
      <h2 className="thesis" style={{ fontSize: 'clamp(1.5rem, 3vw, 2.25rem)' }} id="mech-title">
        Eight decisions, each with the measurement that justified it
      </h2>
      <p className="prose" style={{ marginTop: '1rem' }}>
        Every figure below was measured on this stack, and most are re-checked by the
        pipeline on each push. Where something was learned by breaking it, the failure
        is quoted rather than paraphrased.
      </p>

      {MECHANISMS.map((m) => (
        <article className="mech" key={m.title}>
          <div>
            <p className="eyebrow">{m.eyebrow}</p>
            <h3>{m.title}</h3>
            <p className="prose">{m.prose}</p>
            <p className="where">
              <a href={`${REPO}/blob/main/${m.where.split(' —')[0]}`} target="_blank" rel="noreferrer">
                {m.where}
              </a>
            </p>
          </div>
          <dl className="evidence" style={{ '--evidence-pen': m.pen }}>
            {m.evidence.map((item) => (
              <div key={item.label}>
                <dt>{item.label}</dt>
                <dd>
                  {item.figure && <span className="figure">{item.figure}</span>}
                  {item.code && <code>{item.code}</code>}
                  {item.text && <span>{item.text}</span>}
                </dd>
              </div>
            ))}
          </dl>
        </article>
      ))}
    </section>
  );
}
