// Role-based navigation and route gating. Pure, so it is testable without rendering.
import type { UserInfo } from './auth'

/** admin = the whole platform; a missing role means an older gateway, which only had admins. */
export function isAdminUser(user: Pick<UserInfo, 'role'> | null | undefined): boolean {
  return !user || user.role === undefined || user.role === 'admin'
}

export type NavLeaf = { name: string; href: string; blurb: string; adminOnly?: boolean }
export type NavItem = { name: string; href: string } | { name: string; children: NavLeaf[]; adminOnly?: boolean }

export const NAVIGATION: NavItem[] = [
  { name: 'Dashboard', href: '/dashboard' },
  {
    name: 'Work',
    children: [
      { name: 'Jobs', href: '/jobs', blurb: 'Submit and track training jobs' },
      { name: 'Intelligence', href: '/intelligence', blurb: 'Preflight, evidence, cost and recovery' },
      { name: 'Workflows', href: '/workflows', blurb: 'Multi-step pipelines' },
      { name: 'Tuner', href: '/tuner', blurb: 'Hyperparameter search' },
      { name: 'Experiments', href: '/experiments', blurb: 'Leaderboards for compared runs' },
      { name: 'Workspaces', href: '/workspaces', blurb: 'Jupyter and VS Code environments' },
    ],
  },
  {
    name: 'Models',
    children: [
      { name: 'Models', href: '/models', blurb: 'Registry, stages and promotion' },
      { name: 'Model factory', href: '/model-factory', blurb: 'Fine-tune and serve new open models' },
      { name: 'Datasets', href: '/datasets', blurb: 'Versioned data downloaded into PVCs' },
      { name: 'Data catalog', href: '/data-catalog', blurb: 'Find datasets, owners, schemas and what uses them' },
      { name: 'Lineage', href: '/lineage', blurb: 'Dataset to job to model to service' },
      { name: 'Inference', href: '/inference', blurb: 'Serving and autoscaling' },
      { name: 'LLM gateway', href: '/llm', blurb: 'OpenAI endpoint, API keys and tokens' },
      { name: 'Copilot', href: '/copilot', blurb: 'Ask about your jobs, models and data' },
      { name: 'Playground', href: '/playground', blurb: 'Chat with a model, copy the call' },
      { name: 'Vector indexes', href: '/rag', blurb: 'RAG retrieval over your datasets' },
      { name: 'Agents', href: '/agents', blurb: 'Tool-calling agents on your models' },
    ],
  },
  {
    name: 'Cloud',
    children: [
      { name: 'Catalog', href: '/catalog', blurb: 'GPU SKUs and hourly rates' },
      { name: 'Usage', href: '/usage', blurb: 'GPU hours and estimated cost' },
      { name: 'Reservations', href: '/reservations', blurb: 'Reserve GPU nodes for a team' },
      { name: 'Budgets', href: '/budgets', blurb: 'Spend against limits' },
      { name: 'Invoices', href: '/invoices', blurb: 'Monthly estimated invoices' },
      { name: 'Tenants', href: '/tenants', blurb: 'Tenant accounts and allowed SKUs' },
    ],
  },
  {
    name: 'Platform',
    adminOnly: true,
    children: [
      { name: 'Sovereign AI OS', href: '/sovereign', blurb: 'Gryvia, Zyntra and Netra as one system' },
      { name: 'Nodes', href: '/nodes', blurb: 'GPU nodes and health' },
      { name: 'Quotas', href: '/quotas', blurb: 'Team GPU and budget limits' },
      { name: 'GPU', href: '/gpu', blurb: 'NCCL, stragglers, memory transfers' },
      { name: 'Costs', href: '/costs', blurb: 'GPU spend by team and type' },
    ],
  },
  {
    name: 'Observe',
    adminOnly: true,
    children: [
      { name: 'Network', href: '/network', blurb: 'Service graph, flows and policies' },
      { name: 'Security', href: '/security', blurb: 'Detection rules and alerts' },
      { name: 'Audit', href: '/audit', blurb: 'Who changed what, and who was refused' },
    ],
  },
]

/** Path prefixes only admins may open. /tenants stays open: tenants see a read-only summary of their own. */
export const ADMIN_ONLY_PREFIXES = ['/sovereign', '/nodes', '/quotas', '/costs', '/gpu', '/network', '/security', '/audit']

function hasPrefix(pathname: string, prefix: string): boolean {
  return pathname === prefix || pathname.startsWith(prefix + '/')
}

export function canAccessPath(pathname: string, admin: boolean): boolean {
  return admin || !ADMIN_ONLY_PREFIXES.some((p) => hasPrefix(pathname, p))
}

/** Where a tenant user is sent from a page they may not open. */
export const TENANT_HOME = '/catalog'

/** The nav for a role: admin-only groups and leaves are dropped; a group left empty disappears. */
export function navFor(admin: boolean, nav: NavItem[] = NAVIGATION): NavItem[] {
  return nav.flatMap((item): NavItem[] => {
    if (!('children' in item)) return [item]
    if (item.adminOnly && !admin) return []
    const children = item.children.filter((c) => admin || !c.adminOnly)
    return children.length ? [{ ...item, children }] : []
  })
}
