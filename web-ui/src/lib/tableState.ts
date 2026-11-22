// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

// Pure list logic (search, filter, sort, paginate) behind DataTable; kept separate so it is unit-tested.

export type SortDir = 'asc' | 'desc'

export type TableQuery = {
  search: string
  filters: Record<string, string>
  sortKey?: string
  sortDir: SortDir
  page: number
  pageSize: number
}

export type FilterDef<T> = {
  /** Query/URL name, e.g. "status". */
  name: string
  label: string
  get: (row: T) => string | undefined
  /** Fixed options; when omitted they are built from the values present in the data. */
  options?: string[]
}

export type SortAccessor<T> = (row: T) => string | number | undefined

export function applySearch<T>(rows: T[], search: string, text: (row: T) => string): T[] {
  const q = search.trim().toLowerCase()
  if (!q) return rows
  const terms = q.split(/\s+/)
  return rows.filter((row) => {
    const hay = text(row).toLowerCase()
    return terms.every((t) => hay.includes(t))
  })
}

export function applyFilters<T>(rows: T[], filters: Record<string, string>, defs: FilterDef<T>[]): T[] {
  const active = defs.filter((d) => filters[d.name])
  if (active.length === 0) return rows
  return rows.filter((row) => active.every((d) => (d.get(row) ?? '').toLowerCase() === filters[d.name].toLowerCase()))
}

export function filterOptions<T>(rows: T[], def: FilterDef<T>): string[] {
  if (def.options) return def.options
  const seen = new Set<string>()
  for (const r of rows) {
    const v = def.get(r)
    if (v) seen.add(v)
  }
  return [...seen].sort((a, b) => a.localeCompare(b))
}

export function applySort<T>(rows: T[], key: string | undefined, dir: SortDir, accessors: Record<string, SortAccessor<T>>): T[] {
  const get = key ? accessors[key] : undefined
  if (!get) return rows
  const sign = dir === 'asc' ? 1 : -1
  // Stable: ties keep their original order; missing values sort last in either direction.
  return rows
    .map((row, i) => ({ row, i, v: get(row) }))
    .sort((a, b) => {
      const av = a.v
      const bv = b.v
      if (av === undefined && bv === undefined) return a.i - b.i
      if (av === undefined) return 1
      if (bv === undefined) return -1
      const cmp = typeof av === 'number' && typeof bv === 'number' ? av - bv : String(av).localeCompare(String(bv), undefined, { numeric: true })
      return cmp === 0 ? a.i - b.i : cmp * sign
    })
    .map((x) => x.row)
}

export function paginate<T>(rows: T[], page: number, pageSize: number): { rows: T[]; page: number; pageCount: number; from: number; to: number } {
  const pageCount = Math.max(1, Math.ceil(rows.length / pageSize))
  const safe = Math.min(Math.max(1, page), pageCount)
  const start = (safe - 1) * pageSize
  const slice = rows.slice(start, start + pageSize)
  return { rows: slice, page: safe, pageCount, from: rows.length === 0 ? 0 : start + 1, to: start + slice.length }
}

/** URL <-> query: q, sort, dir, page and f_<name> params. Defaults are omitted to keep URLs short. */
export function queryFromParams(params: URLSearchParams, defs: { name: string }[], defaults: { sortKey?: string; sortDir: SortDir; pageSize: number }): TableQuery {
  const filters: Record<string, string> = {}
  for (const d of defs) {
    const v = params.get(`f_${d.name}`)
    if (v) filters[d.name] = v
  }
  const dir = params.get('dir')
  const page = Number.parseInt(params.get('page') ?? '1', 10)
  return {
    search: params.get('q') ?? '',
    filters,
    sortKey: params.get('sort') ?? defaults.sortKey,
    sortDir: dir === 'asc' || dir === 'desc' ? dir : defaults.sortDir,
    page: Number.isFinite(page) && page > 0 ? page : 1,
    pageSize: defaults.pageSize,
  }
}

export function paramsFromQuery(query: TableQuery, base: URLSearchParams, defs: { name: string }[], defaults: { sortKey?: string; sortDir: SortDir }): URLSearchParams {
  const next = new URLSearchParams(base)
  const set = (k: string, v: string | undefined) => (v ? next.set(k, v) : next.delete(k))
  set('q', query.search.trim() || undefined)
  for (const d of defs) set(`f_${d.name}`, query.filters[d.name])
  set('sort', query.sortKey && query.sortKey !== defaults.sortKey ? query.sortKey : undefined)
  set('dir', query.sortDir !== defaults.sortDir ? query.sortDir : undefined)
  set('page', query.page > 1 ? String(query.page) : undefined)
  return next
}
