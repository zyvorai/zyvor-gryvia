import type {ReactNode} from 'react';
import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';
import InstallCard from '@site/src/components/Marketing/InstallCard';
import s from '@site/src/components/Marketing/Marketing.module.css';

export default function InstallPage(): ReactNode {
  return (
    <Layout title="Install Gryvia" description="Install Gryvia on kind, an existing cluster with Helm, or a fresh k3s GPU server.">
      <header className={s.hero} style={{paddingBottom: '4rem'}}>
        <div className={s.wrap}>
          <h1 className={s.heroTitle}>
            Install <span className={s.gradient}>Gryvia.</span>
          </h1>
          <p className={s.heroSub}>Pick a path. Each one ends at the same dashboard.</p>
        </div>
      </header>
      <main>
        <section className={s.bandDark}>
          <div className={`${s.wrap} ${s.center}`}>
            <div style={{maxWidth: 780, margin: '0 auto'}}>
              <InstallCard />
            </div>
          </div>
        </section>
        <section className={s.band}>
          <div className={s.wrap}>
            <p className={s.eyebrow}>Then</p>
            <h2 className={s.display}>Sign in, submit a job.</h2>
            <div className={s.installGrid}>
              <div className={s.installStep}>
                <h3>1. Sign in</h3>
                <p>Open the dashboard as <code>admin</code>; the password is the <code>auth.apiKey</code> you set. Always set your own for anything reachable from an untrusted network.</p>
              </div>
              <div className={s.installStep}>
                <h3>2. Submit a job</h3>
                <p>Apply a <code>GryviaAIJob</code> with kubectl, the CLI or the dashboard. See the <Link to="/docs/getting-started/quickstart">quickstart</Link> for a first job.</p>
              </div>
              <div className={s.installStep}>
                <h3>3. Turn on what you need</h3>
                <p>Quota gate, Kueue or the <Link to="/docs/guides/NATIVE_SCHEDULER">native queue gate</Link>, fabric-aware scoring: each is an opt-in chart value.</p>
              </div>
            </div>
            <p className={s.fineprint}>
              Gryvia is alpha. Read the <Link to="/docs/getting-started/quickstart">full quickstart</Link> and the{' '}
              <Link to="/docs/guides/AUTH_AND_TLS">auth and TLS guide</Link> before exposing it.
            </p>
          </div>
        </section>
      </main>
    </Layout>
  );
}
