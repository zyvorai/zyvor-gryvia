// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useCallback, useMemo } from 'react'
import { useSearchParams } from 'react-router-dom'
import {
  applyFilters,
  applySearch,
  applySort,
  filterOptions,
  paginate,
  paramsFromQuery,
  queryFromParams,
  type FilterDef,
  type SortAccessor,
  type SortDir,
} from '@/lib/tableState'

type Config<T> = {
  rows: T[] | undefined
  /** Text searched by the search box. */
  searchText: (row: T) => string
  filters?: FilterDef<T>[]
  sortAccessors?: Record<string, SortAccessor<T>>
  defaultSort?: { key: string; dir: SortDir }
  pageSize?: number
}

/** Search/filter/sort/page state for a list, kept in the URL so views are shareable and survive reload. */
export function useTableState<T>({ rows, searchText, filters = [], sortAccessors = {}, defaultSort, pageSize = 25 }: Config<T>) {
  const [params, setParams] = useSearchParams()
  const defaults = useMemo(() => ({ sortKey: defaultSort?.key, sortDir: defaultSort?.dir ?? ('asc' as SortDir), pageSize }), [defaultSort?.key, defaultSort?.dir, pageSize])
  const query = useMemo(() => queryFromParams(params, filters, defaults), [params, filters, defaults])

  const update = useCallback(
    (patch: Partial<typeof query>) => {
      setParams((prev) => paramsFromQuery({ ...queryFromParams(prev, filters, defaults), ...patch }, prev, filters, defaults), { replace: true })
    },
    [setParams, filters, defaults],
  )

  const all = useMemo(() => rows ?? [], [rows])
  const result = useMemo(() => {
    const searched = applySearch(all, query.search, searchText)
    const filtered = applyFilters(searched, query.filters, filters)
    const sorted = applySort(filtered, query.sortKey, query.sortDir, sortAccessors)
    return { filtered: sorted, page: paginate(sorted, query.page, pageSize) }
  }, [all, query, searchText, filters, sortAccessors, pageSize])

  return {
    total: all.length,
    filteredCount: result.filtered.length,
    pageRows: result.page.rows,
    page: result.page.page,
    pageCount: result.page.pageCount,
    from: result.page.from,
    to: result.page.to,
    search: query.search,
    filterValues: query.filters,
    filterDefs: filters,
    filterOptionsFor: (def: FilterDef<T>) => filterOptions(all, def),
    sortKey: query.sortKey,
    sortDir: query.sortDir,
    isFiltered: Boolean(query.search.trim()) || Object.keys(query.filters).length > 0,
    setSearch: (search: string) => update({ search, page: 1 }),
    setFilter: (name: string, value: string) => update({ filters: { ...query.filters, [name]: value }, page: 1 }),
    setPage: (page: number) => update({ page }),
    toggleSort: (key: string) => update({ sortKey: key, sortDir: query.sortKey === key && query.sortDir === 'asc' ? 'desc' : 'asc', page: 1 }),
    reset: () => update({ search: '', filters: {}, page: 1 }),
  }
}

export type TableState<T> = ReturnType<typeof useTableState<T>>
