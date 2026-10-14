import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import HowItWorks from '@site/src/components/Marketing/HowItWorks';
import s from '@site/src/components/Marketing/Marketing.module.css';

const PARTS = [
  {t: 'AI operator', d: 'Reconciles GryviaAIJob: admission, GPU-aware placement, Job or StatefulSet, distributed environment, lifecycle.', to: '/docs/guides/SCHEDULING'},
  {t: 'GPU operator', d: 'Tracks GPU nodes and health, registers nodes from NVIDIA feature-discovery labels.', to: '/docs/guides/GPU_NODES'},
  {t: 'Quota operator', d: 'Per-team quotas, hard budgets, tenants, usage metering and optional per-tenant Kueue queues.', to: '/docs/guides/GPU_AS_A_SERVICE'},
  {t: 'Storage and network operators', d: 'Optional: RDMA and SR-IOV, parallel-filesystem CSI backends. Off by default.', to: '/docs/guides/STORAGE_NETWORK_OPERATORS'},
  {t: 'Network intelligence', d: 'Experimental eBPF flow telemetry and GPU-communication visibility, in its own chart.', to: '/docs/guides/NETWORK_INTELLIGENCE'},
  {t: 'API gateway and dashboard', d: 'One REST API for the CLI, the SDKs and the web dashboard. HTTPS with a generated certificate.', to: '/docs/guides/CLI_GUIDE'},
];

export default function HowItWorksPage(): ReactNode {
  return (
    <Layout title="How Gryvia works" description="The Gryvia control plane: custom resources, operators, admission, placement and observation.">
      <header className={s.hero} style={{paddingBottom: '4rem'}}>
        <div className={s.wrap}>
          <h1 className={s.heroTitle}>
            How it <span className={s.gradient}>works.</span>
          </h1>
          <p className={s.heroSub}>Custom resources in, placed and observed GPU workloads out. Everything runs as operators in your cluster.</p>
        </div>
      </header>
      <main>
        <section className={s.bandDark}>
          <div className={s.wrap}>
            <HowItWorks />
          </div>
        </section>
        <section className={s.band}>
          <div className={s.wrap}>
            <p className={s.eyebrow}>The parts</p>
            <h2 className={s.display}>Operators you can read.</h2>
            <p className={s.lede}>No separate control plane and no agent mesh to learn. Install with Helm; each piece is a Deployment you can inspect.</p>
            <div className={s.archGrid}>
              {PARTS.map((p) => (
                <Link key={p.t} className={s.archCard} to={p.to}>
                  <h3>{p.t}</h3>
                  <p>{p.d}</p>
                </Link>
              ))}
            </div>
            <p className={s.fineprint}>
              Opt-in pieces (admission gate, Kueue integration, native queue gate, fabric-aware scoring) are off by default; with them off,
              behaviour is plain GPU-aware placement.
            </p>
          </div>
        </section>
      </main>
    </Layout>
  );
}
