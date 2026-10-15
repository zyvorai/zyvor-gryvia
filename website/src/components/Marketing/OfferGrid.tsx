import type {ReactNode} from 'react';
import clsx from 'clsx';
import Link from '@docusaurus/Link';
import {LazyMotion, domAnimation, m} from 'framer-motion';
import styles from './Marketing.module.css';

type Tile = {title: string; text: string; to: string; badge?: string; wide?: boolean; art: ReactNode};

const bars = (
  <svg viewBox="0 0 300 80" aria-hidden="true">
    {[0, 1, 2, 3, 4, 5].map((i) => (
      <rect key={i} x={10 + i * 48} y={80 - (20 + ((i * 17) % 50))} width="34" height={20 + ((i * 17) % 50)} rx="8" fill="#0071e3" opacity={0.35 + (i % 3) * 0.2} />
    ))}
  </svg>
);
const gauge = (
  <svg viewBox="0 0 300 80" aria-hidden="true">
    <rect x="10" y="30" width="280" height="20" rx="10" fill="rgba(128,128,128,0.2)" />
    <m.rect x="10" y="30" height="20" rx="10" fill="#34c759" initial={{width: 0}} whileInView={{width: 190}} viewport={{once: true}} transition={{duration: 1.1}} />
    <rect x="198" y="24" width="3" height="32" fill="#ff9f0a" />
  </svg>
);
const flow = (
  <svg viewBox="0 0 300 80" aria-hidden="true">
    <path d="M10 60 C 70 10, 110 70, 160 40 S 250 10, 290 30" fill="none" stroke="#5e5ce6" strokeWidth="3" strokeLinecap="round" />
    <m.circle r="5" fill="#5e5ce6" animate={{cx: [10, 160, 290], cy: [60, 40, 30]}} transition={{duration: 3, repeat: Infinity, ease: 'linear'}} />
  </svg>
);
const stages = (
  <svg viewBox="0 0 300 80" aria-hidden="true">
    {[0, 1, 2, 3].map((i) => (
      <g key={i}>
        <rect x={10 + i * 72} y="20" width="58" height="40" rx="12" fill="#bf5af2" opacity={0.3 + i * 0.2} />
        {i < 3 && <path d={`M${70 + i * 72} 40 h12`} stroke="#bf5af2" strokeWidth="2" />}
      </g>
    ))}
  </svg>
);
const chips = (
  <svg viewBox="0 0 300 80" aria-hidden="true">
    {[0, 1, 2, 3].map((i) => (
      <g key={i}>
        <rect x={10 + i * 72} y="14" width="58" height="52" rx="10" fill="none" stroke="#2997ff" strokeWidth="2" />
        <rect x={18 + i * 72} y={i < 2 ? 22 : 22} width="42" height={i === 1 ? 18 : i === 3 ? 12 : 36} rx="5" fill="#2997ff" opacity="0.7" />
      </g>
    ))}
  </svg>
);
const term = (
  <svg viewBox="0 0 300 80" aria-hidden="true">
    <rect x="10" y="8" width="280" height="64" rx="12" fill="#1d1d1f" />
    <text x="24" y="38" fontFamily="ui-monospace, Menlo, monospace" fontSize="14" fill="#34c759">$ gryvia jobs list</text>
    <m.rect x="24" y="48" width="9" height="14" fill="#f5f5f7" animate={{opacity: [1, 0, 1]}} transition={{duration: 1.1, repeat: Infinity}} />
  </svg>
);

const TILES: Tile[] = [
  {title: 'GPU-aware placement', text: 'Filters and scores nodes on GPU type, RDMA, NVLink, free GPUs and memory, and says why.', to: '/docs/guides/SCHEDULING', art: bars, wide: true},
  {title: 'Queues and gangs', text: 'KAI Scheduler, Kueue or the native gate: guarantees, borrowing, all-or-nothing gangs, preemption and a queue per tenant.', to: '/docs/guides/NATIVE_SCHEDULER', badge: 'KAI · Kueue · native', art: stages, wide: true},
  {title: 'Quotas and budgets', text: 'Per-team GPU quotas, hard budgets and metering, enforced by an operator.', to: '/docs/guides/GPU_AS_A_SERVICE', art: gauge},
  {title: 'Fractional GPUs', text: 'Ask for 0.25 or 0.5 of a GPU: KAI GPU fractions, or a native plan for whole GPU, MIG, MPS or DRA.', to: '/docs/guides/NATIVE_SCHEDULER', art: chips},
  {title: 'Network intelligence', text: 'eBPF flow telemetry and GPU-communication visibility for Prometheus and Grafana.', to: '/docs/guides/NETWORK_INTELLIGENCE', art: flow},
  {title: 'CLI, SDKs, dashboard', text: 'A Rust CLI, Go and Python SDKs and a web dashboard over one API.', to: '/docs/guides/CLI_GUIDE', art: term},
];

export default function OfferGrid(): ReactNode {
  return (
    <LazyMotion features={domAnimation}>
      <div className={styles.bento}>
        {TILES.map((t, i) => (
          <m.div key={t.title} className={clsx(styles.tileCell, t.wide && styles.tileCellWide)}
            initial={{opacity: 0, y: 24}} whileInView={{opacity: 1, y: 0}} viewport={{once: true, amount: 0.3}} transition={{duration: 0.5, delay: (i % 3) * 0.08}}>
            <Link to={t.to} className={styles.tile}>
              {t.badge && <span className={styles.badge}>{t.badge}</span>}
              <h3 className={styles.tileTitle}>{t.title}</h3>
              <p className={styles.tileText}>{t.text}</p>
              <div className={styles.tileArt}>{t.art}</div>
            </Link>
          </m.div>
        ))}
      </div>
    </LazyMotion>
  );
}
