// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// The lineage graph (GET /api/lineage): dataset -> job -> model -> inference service.

export type LineageKind = 'dataset' | 'job' | 'model' | 'service'

export interface LineageNode {
  id: string
  kind: LineageKind
  name: string
  namespace: string
  state?: string | null
  phase?: string | null
  version?: string | null
  stage?: string | null
}

export interface LineageEdge {
  from: string
  to: string
}

export interface LineageGraph {
  nodes: LineageNode[]
  edges: LineageEdge[]
}

export const KIND_ORDER: LineageKind[] = ['dataset', 'job', 'model', 'service']

export const KIND_LABEL: Record<LineageKind, string> = {
  dataset: 'Datasets',
  job: 'Jobs',
  model: 'Models',
  service: 'Inference services',
}

/** Nodes grouped by kind, in pipeline order, each group sorted by name. */
export function columns(g: LineageGraph): { kind: LineageKind; nodes: LineageNode[] }[] {
  return KIND_ORDER.map((kind) => ({
    kind,
    nodes: g.nodes.filter((n) => n.kind === kind).sort((a, b) => a.name.localeCompare(b.name)),
  }))
}

/** Direct upstream (what feeds it) and downstream (what it feeds) nodes of one node. */
export function neighbours(g: LineageGraph, id: string): { upstream: LineageNode[]; downstream: LineageNode[] } {
  const byId = new Map(g.nodes.map((n) => [n.id, n]))
  const pick = (ids: string[]) => ids.map((i) => byId.get(i)).filter((n): n is LineageNode => !!n)
  return {
    upstream: pick(g.edges.filter((e) => e.to === id).map((e) => e.from)),
    downstream: pick(g.edges.filter((e) => e.from === id).map((e) => e.to)),
  }
}

/** Every node reachable from `id` following edges in one direction, excluding `id` itself. */
export function reach(g: LineageGraph, id: string, dir: 'up' | 'down'): LineageNode[] {
  const byId = new Map(g.nodes.map((n) => [n.id, n]))
  const seen = new Set<string>([id])
  const queue = [id]
  while (queue.length) {
    const cur = queue.shift() as string
    for (const e of g.edges) {
      const [near, far] = dir === 'down' ? [e.from, e.to] : [e.to, e.from]
      if (near === cur && !seen.has(far)) {
        seen.add(far)
        queue.push(far)
      }
    }
  }
  seen.delete(id)
  return [...seen].map((i) => byId.get(i)).filter((n): n is LineageNode => !!n)
}
