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

const REPO_DOCS = 'https://github.com/zyvorai/zyvor-gryvia/blob/main/docs';

const NEW = [
  {
    title: 'KAI Scheduler backend',
    text: 'One Helm value hands placement to KAI: hierarchical queues, gangs, GPU fractions, a queue per tenant, and KAI’s reason on every waiting job.',
    to: `${REPO_DOCS}/kai-scheduler.md`,
  },
  {
    title: 'Jupyter and VS Code in the browser',
    text: 'Open a workspace from the dashboard or the CLI through a signed, per-workspace session. Idle workspaces pause and keep their volume.',
    to: `${REPO_DOCS}/workspaces.md`,
  },
  {
    title: 'Submit without YAML',
    text: 'gryvia submit --image pytorch/pytorch --gpu 0.5 -- python train.py. Add --dry-run to see the resource it writes.',
    to: '/docs/guides/CLI_GUIDE',
  },
];

export default function Home(): ReactNode {
  return (
    <Layout
      title="Gryvia: the best GPU platform in the world"
      description="The best GPU platform in the world: GPU-aware placement, KAI Scheduler and Kueue queues, fractional GPUs, browser workspaces, tenants and metering on Kubernetes.">
      <header className={s.hero}>
        <div className={s.wrap}>
          <span className={s.chip}>New · KAI Scheduler backend</span>
          <h1 className={s.heroTitle}>
            The best GPU platform <span className={s.gradient}>in the world.</span>
          </h1>
          <p className={s.heroSub}>
            GPU is the new CPU. Gryvia is its scheduler: place, queue, share and meter every GPU in your Kubernetes
            cluster, and give every team notebooks, IDEs and jobs on top.
          </p>
          <div className={s.ctaRow}>
            <Link className={`${s.pill} ${s.pillPrimary}`} to="/install">Install</Link>
            <Link className={`${s.pill} ${s.pillGhost}`} to="/how-it-works">How it works</Link>
          </div>
        </div>
        <ClusterHero />
      </header>

      <main>
        <section className={s.band}>
          <div className={s.wrap}>
            <Reveal>
              <p className={s.eyebrow}>New</p>
              <h2 className={s.display}>
                Your scheduler. Your notebooks. <span className={s.gradient}>No YAML.</span>
              </h2>
            </Reveal>
            <div className={s.bento}>
              {NEW.map((t) => (
                <div key={t.title} className={s.tileCell}>
                  <Link to={t.to} className={s.tile}>
                    <span className={s.badge}>New</span>
                    <h3 className={s.tileTitle}>{t.title}</h3>
                    <p className={s.tileText}>{t.text}</p>
                  </Link>
                </div>
              ))}
            </div>
          </div>
        </section>

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

        <section className={s.bandDark}>
          <div className={`${s.wrap} ${s.companion}`}>
            <Reveal>
              <p className={s.eyebrow}>Apple Silicon companion</p>
              <h2 className={s.display}>
                Your Macs, <span className={s.gradient}>scheduled.</span>
              </h2>
              <p className={s.lede}>
                A separate control plane for Mac fleets: native Swift worker and app, unified-memory admission, owner-aware idle
                harvesting and signed provenance. It lives in its own repository and does not use the Kubernetes path.
              </p>
              <div className={s.ctaRow}>
                <Link className={s.linkArrow} to="https://github.com/zyvorai/zyvor-gryvia-apple">View zyvor-gryvia-apple</Link>
                <Link className={s.linkArrow} to="https://zyvorai.github.io/zyvor-gryvia-apple/">Companion site</Link>
              </div>
            </Reveal>
            <Reveal>
              <div className={s.companionCard}>
                <dl>
                  <div><dt>Native workers</dt><dd>Swift agent and SwiftUI app for macOS 15+ on Apple Silicon.</dd></div>
                  <div><dt>Memory-aware</dt><dd>Jobs are admitted against each Mac's unified-memory budget.</dd></div>
                  <div><dt>Owner first</dt><dd>Work yields when the Mac's owner returns.</dd></div>
                </dl>
              </div>
            </Reveal>
          </div>
        </section>

        <section className={s.band}>
          <div className={`${s.wrap} ${s.center}`}>
            <Reveal>
              <p className={s.eyebrow}>Where it stands</p>
              <h2 className={s.display}>Every claim, with its test.</h2>
              <p className={s.lede}>
                Gryvia is alpha: the API may change before a stable release. GPU paths are tested on kind with simulated GPUs,
                fake clients and CPU-only clusters, not yet on GPU hardware, and performance figures are targets until
                published benchmarks back them. The README's “What works today” lists what was tested and where.
              </p>
              <div className={s.ctaRow}>
                <Link className={s.linkArrow} to="https://github.com/zyvorai/zyvor-gryvia#what-works-today">What works today</Link>
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
