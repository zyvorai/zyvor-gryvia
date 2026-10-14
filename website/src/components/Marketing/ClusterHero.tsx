import type {ReactNode} from 'react';
import {LazyMotion, domAnimation, m, useReducedMotion} from 'framer-motion';
import styles from './Marketing.module.css';

const NODES = [
  {x: 150, y: 150}, {x: 330, y: 70}, {x: 520, y: 140}, {x: 700, y: 60}, {x: 880, y: 150},
  {x: 250, y: 260}, {x: 440, y: 290}, {x: 640, y: 270}, {x: 800, y: 300},
];
const LINKS: [number, number][] = [[0, 1], [1, 2], [2, 3], [3, 4], [0, 5], [5, 6], [6, 7], [7, 8], [2, 6], [3, 7], [1, 5], [4, 8]];

/** Decorative cluster: GPUs pulse, links carry a travelling dot. Static when reduced motion is on. */
export default function ClusterHero(): ReactNode {
  const reduce = useReducedMotion();
  return (
    <LazyMotion features={domAnimation}>
      <svg className={styles.heroArt} viewBox="0 0 1030 360" role="img" aria-label="A cluster of GPU nodes connected by links, with work moving between them">
        <defs>
          <radialGradient id="zvGlow" cx="50%" cy="50%" r="50%">
            <stop offset="0%" stopColor="#2997ff" stopOpacity="0.55" />
            <stop offset="100%" stopColor="#2997ff" stopOpacity="0" />
          </radialGradient>
          <linearGradient id="zvLine" x1="0" x2="1">
            <stop offset="0%" stopColor="#2997ff" stopOpacity="0.1" />
            <stop offset="50%" stopColor="#5e5ce6" stopOpacity="0.7" />
            <stop offset="100%" stopColor="#bf5af2" stopOpacity="0.1" />
          </linearGradient>
        </defs>
        {LINKS.map(([a, b], i) => (
          <line key={i} x1={NODES[a].x} y1={NODES[a].y} x2={NODES[b].x} y2={NODES[b].y} stroke="url(#zvLine)" strokeWidth="1.5" />
        ))}
        {!reduce &&
          LINKS.map(([a, b], i) => (
            <m.circle
              key={`d${i}`}
              r="3.5"
              fill="#f5f5f7"
              initial={{cx: NODES[a].x, cy: NODES[a].y, opacity: 0}}
              animate={{cx: [NODES[a].x, NODES[b].x], cy: [NODES[a].y, NODES[b].y], opacity: [0, 1, 0]}}
              transition={{duration: 2.6, repeat: Infinity, delay: i * 0.35, ease: 'easeInOut'}}
            />
          ))}
        {NODES.map((n, i) => (
          <g key={i}>
            <m.circle
              cx={n.x}
              cy={n.y}
              r="46"
              fill="url(#zvGlow)"
              animate={reduce ? undefined : {opacity: [0.35, 1, 0.35]}}
              transition={{duration: 3.2, repeat: Infinity, delay: i * 0.3}}
            />
            <rect x={n.x - 20} y={n.y - 14} width="40" height="28" rx="7" fill="#1d1d1f" stroke="#2997ff" strokeOpacity="0.7" />
            <rect x={n.x - 12} y={n.y - 6} width="10" height="12" rx="2" fill="#2997ff" fillOpacity="0.85" />
            <rect x={n.x + 2} y={n.y - 6} width="10" height="12" rx="2" fill="#5e5ce6" fillOpacity="0.85" />
          </g>
        ))}
      </svg>
    </LazyMotion>
  );
}
