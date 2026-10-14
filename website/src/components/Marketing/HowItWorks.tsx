import {useState} from 'react';
import type {ReactNode} from 'react';
import clsx from 'clsx';
import {LazyMotion, domAnimation, m} from 'framer-motion';
import styles from './Marketing.module.css';

export type Step = {title: string; text: string; stage: string; detail: string};

export const STEPS: Step[] = [
  {
    title: 'Describe the work.',
    text: 'Submit a GryviaAIJob with kubectl, the CLI, the dashboard or the API. GPUs, image, priority and an optional queue are plain fields on a custom resource.',
    stage: 'Submit',
    detail: 'GryviaAIJob',
  },
  {
    title: 'Admit it, or hold it.',
    text: 'An opt-in gate checks quotas and hard budgets first. Gang admission and queues come from Kueue, or from the alpha native queue gate: all pods fit together, or the job waits.',
    stage: 'Admit',
    detail: 'quota · budget · queue',
  },
  {
    title: 'Place it where it fits.',
    text: 'The operator filters and scores nodes on GPU type, RDMA, NVLink, free GPUs and memory, and records the choice and the reason in the job status.',
    stage: 'Place',
    detail: 'GPU-aware scoring',
  },
  {
    title: 'Run it. Watch it.',
    text: 'It becomes an Indexed Job or StatefulSet with the distributed environment set. Metrics, checkpoint guards and network telemetry (experimental) show how it is doing.',
    stage: 'Run',
    detail: 'Job · metrics · checkpoints',
  },
];

function Stage({active}: {active: number}): ReactNode {
  return (
    <svg viewBox="0 0 560 420" role="img" aria-label={`Pipeline stage ${active + 1} of 4: ${STEPS[active].stage}`}>
      {STEPS.map((s, i) => {
        const y = 20 + i * 100;
        const on = i === active;
        return (
          <g key={s.stage}>
            {i > 0 && <line x1="280" y1={y - 22} x2="280" y2={y} stroke="#2997ff" strokeOpacity={i <= active ? 0.9 : 0.2} strokeWidth="2" />}
            <m.rect
              x="70"
              y={y}
              width="420"
              height="68"
              rx="18"
              fill="#15151a"
              animate={{stroke: on ? '#2997ff' : '#2a2a30', strokeWidth: on ? 2.5 : 1, scale: on ? 1.03 : 1}}
              style={{transformOrigin: '280px ' + (y + 34) + 'px'}}
              transition={{duration: 0.4}}
            />
            <circle cx="112" cy={y + 34} r="14" fill={on ? '#2997ff' : '#2a2a30'} />
            <text x="112" y={y + 39} textAnchor="middle" fontSize="14" fontWeight="700" fill="#fff">{i + 1}</text>
            <text x="144" y={y + 31} fontSize="20" fontWeight="650" fill={on ? '#f5f5f7' : '#86868b'}>{s.stage}</text>
            <text x="144" y={y + 53} fontSize="13" fill={on ? '#a1a1a6' : '#5a5a60'}>{s.detail}</text>
          </g>
        );
      })}
    </svg>
  );
}

export default function HowItWorks(): ReactNode {
  const [active, setActive] = useState(0);
  return (
    <LazyMotion features={domAnimation}>
      <div className={styles.story}>
        <div className={styles.steps}>
          {STEPS.map((s, i) => (
            <m.div
              key={s.title}
              className={clsx(styles.step, i === active && styles.stepActive)}
              onViewportEnter={() => setActive(i)}
              viewport={{amount: 0.6}}>
              <span className={styles.stepNum}>Step {i + 1}</span>
              <h3 className={styles.stepTitle}>{s.title}</h3>
              <p className={styles.stepText}>{s.text}</p>
            </m.div>
          ))}
        </div>
        <div className={styles.stage}>
          <Stage active={active} />
        </div>
      </div>
    </LazyMotion>
  );
}
