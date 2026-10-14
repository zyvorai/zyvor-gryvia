import {useState} from 'react';
import type {ReactNode} from 'react';
import clsx from 'clsx';
import styles from './Marketing.module.css';

type Path = {id: string; label: string; code: string; note: ReactNode};

export const PATHS: Path[] = [
  {
    id: 'kind',
    label: 'Try it, no GPUs',
    code: `git clone https://github.com/zyvorai/zyvor-gryvia && cd zyvor-gryvia
./scripts/kind-demo.sh
kubectl -n gryvia-system port-forward svc/gryvia-ui 8443:443`,
    note: 'Creates a kind cluster with fictional demo data so the dashboard is populated. Open https://localhost:8443 and accept the self-signed certificate.',
  },
  {
    id: 'helm',
    label: 'Your cluster',
    code: `helm install gryvia oci://ghcr.io/zyvorai/charts/gryvia \\
  --namespace gryvia-system --create-namespace \\
  --set auth.apiKey='a-long-random-secret'
kubectl -n gryvia-system port-forward svc/gryvia-ui 8443:443`,
    note: 'Needs kubectl, Helm 3.14+ and Kubernetes 1.30+. Sign in as admin with the API key you set.',
  },
  {
    id: 'k3s',
    label: 'Fresh GPU server',
    code: `sudo ./scripts/install-k3s-gpu.sh server`,
    note: 'Ubuntu 22.04/24.04 with an NVIDIA GPU: installs k3s, Gryvia and the NVIDIA GPU Operator. Covered by dry-run and GPU-less CI tests; not yet run on real GPU hardware.',
  },
];

export default function InstallCard(): ReactNode {
  const [id, setId] = useState(PATHS[0].id);
  const [copied, setCopied] = useState(false);
  const p = PATHS.find((x) => x.id === id) ?? PATHS[0];
  const copy = () => {
    try {
      void navigator.clipboard.writeText(p.code).then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      });
    } catch {
      /* clipboard unavailable: the text stays selectable */
    }
  };
  return (
    <div>
      <div className={styles.tabs} role="tablist" aria-label="Install path">
        {PATHS.map((x) => (
          <button key={x.id} type="button" role="tab" aria-selected={x.id === id} className={clsx(styles.tab, x.id === id && styles.tabOn)} onClick={() => setId(x.id)}>
            {x.label}
          </button>
        ))}
      </div>
      <div className={styles.term}>
        <div className={styles.termBar}>
          <span className={styles.termDot} style={{background: '#ff5f57'}} />
          <span className={styles.termDot} style={{background: '#febc2e'}} />
          <span className={styles.termDot} style={{background: '#28c840'}} />
          <button type="button" className={styles.termCopy} onClick={copy}>{copied ? 'Copied' : 'Copy'}</button>
        </div>
        <pre className={styles.termBody}>{p.code}</pre>
      </div>
      <p className={styles.termNote}>{p.note}</p>
    </div>
  );
}
