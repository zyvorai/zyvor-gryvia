// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// Layers a workflow's steps by dependency depth (topological levels).

type StepLike = { name: string; dependsOn?: string[] }

/**
 * Level 0 = steps with no (known) dependencies; every other step sits one level below its deepest
 * dependency. Order within a level follows the input order. Unknown dependencies are ignored and
 * dependency cycles do not loop forever (the cycle edge is dropped).
 */
export function dagLayers<T extends StepLike>(steps: T[]): T[][] {
  const byName = new Map(steps.map((s) => [s.name, s]))
  const memo = new Map<string, number>()
  const visiting = new Set<string>()

  const level = (name: string): number => {
    const cached = memo.get(name)
    if (cached !== undefined) return cached
    if (visiting.has(name)) return 0
    visiting.add(name)
    let depth = 0
    for (const dep of byName.get(name)?.dependsOn ?? []) {
      if (dep !== name && byName.has(dep)) depth = Math.max(depth, level(dep) + 1)
    }
    visiting.delete(name)
    memo.set(name, depth)
    return depth
  }

  const layers: T[][] = []
  for (const step of steps) {
    const l = level(step.name)
    ;(layers[l] ??= []).push(step)
  }
  return layers.filter(Boolean)
}

export type DagIssue = {
  kind: 'duplicate' | 'unknown' | 'self' | 'cycle'
  /** Names of the steps the problem should be shown on. */
  steps: string[]
  message: string
}

/**
 * The gateway's DAG rules, run live in the form: duplicate names, dependencies on steps that do
 * not exist, self-dependencies and cycles. Blank names are skipped (the name field reports those).
 */
export function validateDag(steps: StepLike[]): DagIssue[] {
  const issues: DagIssue[] = []
  const named = steps.filter((s) => s.name !== '')
  const seen = new Set<string>()
  const dup = new Set<string>()
  for (const s of named) (seen.has(s.name) ? dup : seen).add(s.name)
  for (const name of dup) issues.push({ kind: 'duplicate', steps: [name], message: `Step name "${name}" is used more than once.` })

  const names = new Set(named.map((s) => s.name))
  for (const s of named) {
    for (const d of s.dependsOn ?? []) {
      if (d === s.name) issues.push({ kind: 'self', steps: [s.name], message: `Step "${s.name}" cannot depend on itself.` })
      else if (!names.has(d)) issues.push({ kind: 'unknown', steps: [s.name], message: `Step "${s.name}" depends on unknown step "${d}".` })
    }
  }

  // Kahn's algorithm over the valid edges only; whatever is left over is in or behind a cycle.
  const remaining = new Map<string, Set<string>>()
  for (const s of named) {
    if (remaining.has(s.name)) continue
    remaining.set(s.name, new Set((s.dependsOn ?? []).filter((d) => d !== s.name && names.has(d))))
  }
  for (;;) {
    const ready = [...remaining].filter(([, deps]) => deps.size === 0).map(([n]) => n)
    if (ready.length === 0) break
    for (const n of ready) remaining.delete(n)
    for (const deps of remaining.values()) for (const n of ready) deps.delete(n)
  }
  if (remaining.size > 0) {
    const members = [...remaining.keys()]
    issues.push({ kind: 'cycle', steps: members, message: `Dependencies form a cycle: ${members.join(', ')}.` })
  }
  return issues
}

/**
 * Plain-text version of the layered drawing for screen readers: one line per step in execution
 * order, saying its stage and what it waits for. `extra` appends per-step detail (e.g. status).
 */
export function describeSteps<T extends StepLike>(steps: T[], extra?: (step: T) => string | undefined): string[] {
  const known = new Set(steps.map((s) => s.name))
  return dagLayers(steps).flatMap((layer, i) =>
    layer.map((s) => {
      const deps = (s.dependsOn ?? []).filter((d) => d !== s.name && known.has(d))
      const tail = extra?.(s)
      return `${s.name}: stage ${i + 1}, ${deps.length > 0 ? `runs after ${deps.join(', ')}` : 'no dependencies'}${tail ? `, ${tail}` : ''}`
    }),
  )
}
