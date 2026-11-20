// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

// The data catalog (GET /api/data-catalog, PUT /api/data-catalog/{name}): datasets with owner, tags, schema,
// freshness and what depends on them.

export interface CatalogColumn {
  name: string
  type?: string
  description?: string
}

export interface CatalogEntry {
  name: string
  namespace: string
  description: string
  type: string
  license: string
  owner: string
  tags: string[]
  columns: CatalogColumn[]
  marking?: string | null
  state?: string | null
  version?: string | null
  files?: number | null
  bytes?: number | null
  updated?: string | null
  /** "job/train", "model/bert", "service/svc": everything downstream of the dataset. */
  usedBy: string[]
}

export interface CatalogResponse {
  items: CatalogEntry[]
  facets: { tags: Record<string, number>; owners: Record<string, number> }
}

export interface CatalogEdit {
  owner: string
  description: string
  tags: string[]
  columns: CatalogColumn[]
}

/** One column per line: "name", "name: type" or "name: type - description". Blank lines are skipped. */
export function parseColumns(text: string): CatalogColumn[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)
    .map((line) => {
      const [head, ...rest] = line.split(' - ')
      const [name, ...type] = head.split(':')
      const col: CatalogColumn = { name: name.trim() }
      if (type.join(':').trim()) col.type = type.join(':').trim()
      if (rest.join(' - ').trim()) col.description = rest.join(' - ').trim()
      return col
    })
}

export function formatColumns(cols: CatalogColumn[]): string {
  return cols.map((c) => `${c.name}${c.type ? `: ${c.type}` : ''}${c.description ? ` - ${c.description}` : ''}`).join('\n')
}

/** Comma separated tags: trimmed, lowercased, deduplicated, blanks dropped. */
export function parseTags(text: string): string[] {
  return Array.from(new Set(text.split(',').map((t) => t.trim().toLowerCase()).filter(Boolean)))
}

/** "2 jobs, 1 model" for what is downstream of a dataset, or "Not used yet". */
export function usedByLabel(usedBy: string[]): string {
  if (usedBy.length === 0) return 'Not used yet'
  const counts = new Map<string, number>()
  for (const u of usedBy) {
    const kind = u.split('/')[0]
    counts.set(kind, (counts.get(kind) ?? 0) + 1)
  }
  return ['job', 'model', 'service']
    .filter((k) => counts.has(k))
    .map((k) => `${counts.get(k)} ${k}${counts.get(k) === 1 ? '' : 's'}`)
    .join(', ')
}
