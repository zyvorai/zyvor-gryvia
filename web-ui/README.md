# Gryvia Web UI

React dashboard for the Gryvia GPU orchestration platform. The look follows the sibling `netra` project:
its stylesheets are copied verbatim into `src/styles/netra*.css` (do not hand-edit them) and Gryvia-specific
additions live in `src/styles/gryvia.css`. Light or dark follows the system setting and can be toggled.

> **Status.** The dashboard is exercised against the kind demo (`scripts/kind-demo.sh`, fictional data) in CI, and has component tests. Pages for Workspaces, Models, Inference, Workflows and Tuner create, list and delete the matching custom resources through the gateway, but no operator reconciles those kinds yet, so creating one does not start any work. Network, security and GPU-communication pages depend on collectors and Hubble/Prometheus paths that are not fully implemented and are often empty. Dashboard figures on real GPU hardware are unverified.

## Features

- **Dashboard** - cluster stats, GPU utilization, job pipeline, links into the pages behind each number
- **Jobs** - searchable, filterable, sortable table (state lives in the URL, so views are shareable); submit, clone,
  delete; job details show conditions, pods, logs (tail) and events
- **Workspaces, Models, Inference, Workflows, Tuner** - list pages (CRUD on the CRs only; see Status) with the same table controls, detail panels,
  create forms and confirmations; structured editors for tuner parameter spaces and workflow steps (with DAG
  validation) and an "Advanced: edit as JSON" escape hatch
- **Quotas, Nodes, Costs** - team quotas and budgets, GPU node health, spend to date; drill-down links to the jobs
  they own
- **Network and Security** - interactive service graph, observed flows, flow policies (`?new=1` prefill), trace
  sessions, security policies, GPU communication analysis. Empty states name the operator or collector that feeds the
  data instead of showing reassuring zeros
- **Auth** - login (admin credentials or a bearer key), session-expiry handling that returns you to the page you were on
- **Accessibility** - skip link, focus moves to the page on navigation, modal focus trap and Escape, keyboard-operable
  rows, `aria-sort` headers, per-page document titles
- **Resilience** - in-shell error boundary with retry, stale-data indicator, toasts for every mutation

## Tech Stack

- **React 19** + **TypeScript** on **Vite**
- **React Router** (lazy routes), **TanStack Query** (polling; no retry on 4xx)
- **Recharts** for charts
- Plain global CSS with netra design tokens (no Tailwind)
- **Vitest** + Testing Library (jsdom)

## Development

```bash
cd web-ui
npm install
npm run dev      # http://localhost:5173, proxies /api to a gateway on localhost:8001
npm run build    # production build to dist/
npm run lint     # ESLint, zero warnings allowed
npm test         # unit and component tests
npx tsc --noEmit # type check
```

## API

The UI talks only to the Gryvia API Gateway. All endpoints require `Authorization: Bearer <token>`; the token comes
from the login screen (stored in `localStorage` as `gryvia_token`). See
[`services/api-gateway/README.md`](../services/api-gateway/README.md) and the
[API reference](../website/docs/developer-guide/api-reference.md) for the routes, including the job runtime routes
`GET /api/jobs/{name}/pods|logs|events`.

Typed responses and request bodies live in `src/lib/api.ts`.

## Deployment

The UI is served over HTTPS by nginx (port 8443, NodePort 32443 by default) and proxies `/api/` to the gateway.
The certificate is self-signed and shared through the `gryvia-tls` Secret. Deploy everything with:

```bash
./scripts/deploy-remote.sh <host> <ssh-user>
```

## Project Structure

```
web-ui/src/
  components/   Layout (grouped nav), Modal, ConfirmDialog, DataTable, StateViews (error/empty/skeleton),
                Progress, CopyButton, ParamSpaceEditor, StepEditor, ErrorBoundary, AuthProvider, ...
  hooks/        useTableState (URL-backed table state), useDocumentTitle
  lib/          api client, phase (single status vocabulary), format (dates, bytes, money),
                tableState, notify, errors, authEvents, and per-page logic (jobs, network, security,
                tuner, paramSpace, workflowSteps, dag, ...) with tests beside each file
  pages/        one file per route (see App.tsx)
  styles/       netra.css, netra-story.css (verbatim copies) and gryvia.css (extras)
  App.tsx       routes, protected layout, session guard, query client defaults
```

## License

Zyvor Production License v1.0 (LicenseRef-Zyvor-Production-1.0). See the repository LICENSE.