import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import ClusterHero from '@site/src/components/Marketing/ClusterHero';
import HowItWorks from '@site/src/components/Marketing/HowItWorks';
import OfferGrid from '@site/src/components/Marketing/OfferGrid';
import QueueDiagram from '@site/src/components/Marketing/QueueDiagram';
import InstallCard from '@site/src/components/Marketing/InstallCard';
import Reveal from '@site/src/components/Reveal';
import s from '@site/src/components/Marketing/Marketing.module.css';

export default function Home(): ReactNode {
  return (
    <Layout
      title="Gryvia: open GPU orchestration for Kubernetes"
      description="Open GPU orchestration for Kubernetes: GPU-aware placement, queues, quotas, network intelligence and ML workflows as operators and CRDs.">
      <header className={s.hero}>
        <div className={s.wrap}>
          <span className={s.chip}>Alpha · gryvia.io/v1alpha1</span>
          <h1 className={s.heroTitle}>
            GPUs, <span className={s.gradient}>orchestrated.</span>
          </h1>
          <p className={s.heroSub}>
            Place, queue and observe GPU workloads with operators and custom resources you already know how to run on Kubernetes.
          </p>
          <div className={s.ctaRow}>
            <Link className={`${s.pill} ${s.pillPrimary}`} to="/install">Install</Link>
            <Link className={`${s.pill} ${s.pillGhost}`} to="/how-it-works">How it works</Link>
          </div>
        </div>
        <ClusterHero />
      </header>

      <main>
        <section className={s.bandDark}>
          <div className={s.wrap}>
            <p className={s.eyebrow}>How it works</p>
            <h2 className={s.display}>From a YAML file to a running job.</h2>
            <p className={s.lede}>Four steps, all visible on the job itself. No separate control plane.</p>
            <div style={{marginTop: '3rem'}}>
              <HowItWorks />
            </div>
          </div>
        </section>

        <section className={s.band}>
          <div className={s.wrap}>
            <Reveal>
              <p className={s.eyebrow}>What you get</p>
              <h2 className={s.display}>One platform. Six operators.</h2>
              <p className={s.lede}>
                Everything is a CRD with a controller behind it. Turn on what you need, leave the rest off.
              </p>
            </Reveal>
            <OfferGrid />
          </div>
        </section>

        <section className={s.bandDark}>
          <div className={s.wrap}>
            <Reveal>
              <p className={s.eyebrow}>Native queue scheduler · alpha</p>
              <h2 className={s.display}>
                Share a cluster <span className={s.gradient}>fairly.</span>
              </h2>
              <p className={s.lede}>
                Queue guarantees and ceilings, borrowing, all-or-nothing gangs and priority preemption, driven by one ConfigMap.
                Opt-in, unit-tested, and not combinable with Kueue.
              </p>
            </Reveal>
            <QueueDiagram />
            <p className={s.fineprint}>
              Illustration of the documented behaviour. It does not reorder waiting jobs and is not yet verified on GPUs.{' '}
              <Link className={s.linkArrow} to="/docs/guides/NATIVE_SCHEDULER">Read the guide</Link>
            </p>
          </div>
        </section>

        <section className={s.bandAlt}>
          <div className={`${s.wrap} ${s.center}`}>
            <Reveal>
              <p className={s.eyebrow}>Install</p>
              <h2 className={s.display}>Up in minutes.</h2>
              <p className={s.lede}>Start on a laptop with no GPUs, or install on the cluster you already have.</p>
              <div style={{maxWidth: 760, margin: '2.5rem auto 0'}}>
                <InstallCard />
              </div>
              <p className={s.fineprint}>
                <Link className={s.linkArrow} to="/install">All install options</Link>
              </p>
            </Reveal>
          </div>
        </section>

        <section className={s.band}>
          <div className={`${s.wrap} ${s.center}`}>
            <Reveal>
              <h2 className={s.display}>Early, and honest about it.</h2>
              <p className={s.lede}>
                Gryvia is alpha. The API may change before a stable release, most GPU behaviour is unit-tested or run on CPU-only
                clusters, and performance figures are targets until published benchmarks back them.
              </p>
              <div className={s.ctaRow}>
                <Link className={s.linkArrow} to="/docs/guides/ROADMAP">Read the roadmap</Link>
                <Link className={s.linkArrow} to="https://github.com/zyvorai/zyvor-gryvia">View on GitHub</Link>
              </div>
            </Reveal>
          </div>
        </section>
      </main>
    </Layout>
  );
}
