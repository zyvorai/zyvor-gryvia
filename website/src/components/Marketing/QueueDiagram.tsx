import {useEffect, useState} from 'react';
import type {ReactNode} from 'react';
import clsx from 'clsx';
import {LazyMotion, domAnimation, m, useReducedMotion} from 'framer-motion';
import styles from './Marketing.module.css';

type Job = {id: string; color: string};
type Frame = {slots: (string | null)[]; waiting: string[]; caption: string};

const JOBS: Record<string, Job> = {
  A: {id: 'A', color: '#2997ff'},
  B: {id: 'B', color: '#30d158'},
  C: {id: 'C', color: '#ff9f0a'},
  G: {id: 'G', color: '#bf5af2'},
  D: {id: 'D', color: '#ff375f'},
};

// 4 GPUs of capacity. A is research and preemptible, D is production at higher priority.
const FRAMES: Frame[] = [
  {slots: ['A', 'A', 'B', 'B'], waiting: [], caption: 'Two jobs fit inside the cluster. Both are admitted, each counted against its queue.'},
  {slots: ['A', 'A', 'B', 'B'], waiting: ['C'], caption: 'Job C needs a GPU. Nothing is free and nothing may be preempted, so it queues, with the reason on the job.'},
  {slots: ['A', 'A', 'B', 'B'], waiting: ['C', 'G'], caption: 'A gang of three is all-or-nothing. It waits rather than starting a partial set.'},
  {slots: ['D', 'D', 'B', 'B'], waiting: ['C', 'G'], caption: 'A higher-priority job arrives. It may evict a lower-priority job that opted in with native-preemptible. B is untouched.'},
];

export default function QueueDiagram(): ReactNode {
  const [i, setI] = useState(0);
  const reduce = useReducedMotion();
  useEffect(() => {
    if (reduce) return;
    const t = setInterval(() => setI((v) => (v + 1) % FRAMES.length), 4200);
    return () => clearInterval(t);
  }, [reduce]);
  const f = FRAMES[i];
  return (
    <LazyMotion features={domAnimation}>
      <div className={styles.queueCard}>
        <svg viewBox="0 0 640 200" role="img" aria-label="Four GPU slots filling, queueing and preempting as jobs arrive">
          <text x="10" y="24" fontSize="14" fill="#86868b">Cluster: 4 GPUs</text>
          {f.slots.map((s, k) => (
            <m.rect key={k} x={10 + k * 110} y="36" width="98" height="64" rx="14"
              animate={{fill: s ? JOBS[s].color : '#1d1d1f', opacity: s ? 0.95 : 1}} transition={{duration: 0.5}} stroke="#2a2a30" />
          ))}
          {f.slots.map((s, k) => (
            <text key={`t${k}`} x={10 + k * 110 + 49} y="75" textAnchor="middle" fontSize="20" fontWeight="700" fill="#fff">{s ?? ''}</text>
          ))}
          <text x="10" y="140" fontSize="14" fill="#86868b">Waiting</text>
          {f.waiting.length === 0 && <text x="10" y="170" fontSize="14" fill="#5a5a60">empty</text>}
          {f.waiting.map((w, k) => (
            <m.g key={w} initial={{opacity: 0, y: 10}} animate={{opacity: 1, y: 0}} transition={{duration: 0.4}}>
              <rect x={10 + k * 110} y="150" width="98" height="36" rx="12" fill="none" stroke={JOBS[w].color} strokeWidth="2" strokeDasharray="6 5" />
              <text x={10 + k * 110 + 49} y="174" textAnchor="middle" fontSize="16" fontWeight="700" fill={JOBS[w].color}>{w}</text>
            </m.g>
          ))}
        </svg>
        <p className={styles.queueCaption} aria-live="polite">{f.caption}</p>
        <div className={styles.queueDots}>
          {FRAMES.map((_, k) => (
            <button key={k} type="button" aria-label={`Show step ${k + 1}`} className={clsx(styles.queueDot, k === i && styles.queueDotOn)} onClick={() => setI(k)} />
          ))}
        </div>
      </div>
    </LazyMotion>
  );
}
