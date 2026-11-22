// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import type { ReactNode } from 'react'
import type { TableState } from '@/hooks/useTableState'
import { EmptyState } from './StateViews'

export type Column<T> = {
  key: string
  header: string
  render: (row: T) => ReactNode
  /** Must match a key in the hook's sortAccessors to be sortable. */
  sortable?: boolean
  /** Right-aligned tabular figures. */
  numeric?: boolean
}

type DataTableProps<T> = {
  /** Visually hidden table caption (screen readers). */
  caption: string
  columns: Column<T>[]
  state: TableState<T>
  rowKey: (row: T) => string
  onRowClick?: (row: T) => void
  /** Shown when there is no data at all (as opposed to no match for the filters). */
  empty: ReactNode
  /** Extra controls placed after the search/filters (buttons etc). */
  actions?: ReactNode
  searchLabel?: string
}

/** Search + filters + sortable headers + pagination over a list, driven by useTableState (URL-backed). */
export default function DataTable<T>({ caption, columns, state, rowKey, onRowClick, empty, actions, searchLabel = 'Search' }: DataTableProps<T>) {
  const { total, filteredCount } = state
  return (
    <>
      <div className="toolbar" role="search">
        <label>
          {searchLabel}
          <input type="search" value={state.search} onChange={(e) => state.setSearch(e.target.value)} placeholder="Search…" />
        </label>
        {state.filterDefs.map((def) => (
          <label key={def.name}>
            {def.label}
            <select value={state.filterValues[def.name] ?? ''} onChange={(e) => state.setFilter(def.name, e.target.value)}>
              <option value="">All</option>
              {state.filterOptionsFor(def).map((o) => (
                <option key={o} value={o}>
                  {o}
                </option>
              ))}
            </select>
          </label>
        ))}
        {state.isFiltered && (
          <button type="button" className="btn-secondary" onClick={state.reset}>
            Clear filters
          </button>
        )}
        {actions}
      </div>

      {total === 0 ? (
        empty
      ) : filteredCount === 0 ? (
        <EmptyState
          title="Nothing matches these filters."
          action={
            <button type="button" className="btn-secondary" onClick={state.reset}>
              Clear filters
            </button>
          }
        />
      ) : (
        <>
          <div className="table-wrap">
            <table>
              <caption className="sr-only">{caption}</caption>
              <thead>
                <tr>
                  {columns.map((c) => {
                    const sorted = state.sortKey === c.key
                    return (
                      <th
                        key={c.key}
                        scope="col"
                        className={c.numeric ? 'num' : undefined}
                        aria-sort={c.sortable ? (sorted ? (state.sortDir === 'asc' ? 'ascending' : 'descending') : 'none') : undefined}
                      >
                        {c.sortable ? (
                          <button type="button" className="th-sort" onClick={() => state.toggleSort(c.key)}>
                            {c.header}
                            <span aria-hidden="true">{sorted ? (state.sortDir === 'asc' ? ' ▲' : ' ▼') : ''}</span>
                          </button>
                        ) : (
                          c.header
                        )}
                      </th>
                    )
                  })}
                </tr>
              </thead>
              <tbody>
                {state.pageRows.map((row) => (
                  <tr
                    key={rowKey(row)}
                    data-clickable={onRowClick ? '' : undefined}
                    tabIndex={onRowClick ? 0 : undefined}
                    onClick={onRowClick ? () => onRowClick(row) : undefined}
                    onKeyDown={
                      onRowClick
                        ? (e) => {
                            if ((e.key === 'Enter' || e.key === ' ') && e.target === e.currentTarget) {
                              e.preventDefault()
                              onRowClick(row)
                            }
                          }
                        : undefined
                    }
                  >
                    {columns.map((c) => (
                      <td key={c.key} className={c.numeric ? 'num' : undefined}>
                        {c.render(row)}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="toolbar toolbar-between">
            <span className="faint" role="status">
              Showing {state.from}–{state.to} of {filteredCount}
              {filteredCount !== total ? ` (${total} total)` : ''}
            </span>
            {state.pageCount > 1 && (
              <nav aria-label="Pagination" className="toolbar">
                <button type="button" className="btn-secondary" disabled={state.page <= 1} onClick={() => state.setPage(state.page - 1)}>
                  Previous
                </button>
                <span className="faint">
                  Page {state.page} of {state.pageCount}
                </span>
                <button type="button" className="btn-secondary" disabled={state.page >= state.pageCount} onClick={() => state.setPage(state.page + 1)}>
                  Next
                </button>
              </nav>
            )}
          </div>
        </>
      )}
    </>
  )
}
