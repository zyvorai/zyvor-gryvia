// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useMemo } from 'react'
import { LineChart, Line, XAxis, YAxis, CartesianGrid, Tooltip, Legend, ResponsiveContainer } from 'recharts'
import { SERIES_COLORS } from '@/lib/chartColors'

interface GPUDataPoint {
  time: string
  [nodeKey: string]: string | number
}

interface GPUChartProps {
  data?: GPUDataPoint[]
}

/**
 * Utilization per GPU. The gateway returns one snapshot per GPU, so rows sharing a timestamp are
 * merged into one; with a single moment there is no line to draw, so the current value of each GPU
 * is shown as a bar instead.
 */
export default function GPUChart({ data }: GPUChartProps) {
  const { rows, keys } = useMemo(() => {
    const byTime = new Map<string, GPUDataPoint>()
    const keySet = new Set<string>()
    for (const row of data ?? []) {
      const merged = byTime.get(row.time) ?? { time: row.time }
      for (const [k, v] of Object.entries(row)) {
        if (k === 'time' || k === 'timestamp') continue
        merged[k] = v
        keySet.add(k)
      }
      byTime.set(row.time, merged)
    }
    return { rows: [...byTime.values()], keys: [...keySet] }
  }, [data])

  if (rows.length === 0) {
    return <p className="empty-state">No GPU utilization data available</p>
  }

  if (rows.length === 1) {
    const latest = rows[0]
    return (
      <div className="stack">
        {keys.map((k) => {
          const value = Number(latest[k]) || 0
          return (
            <div key={k}>
              <div className="list-row">
                <div className="grow">
                  <b>{k}</b>
                </div>
                <span className="mono muted">{Math.round(value)}%</span>
              </div>
              <div className={`progress${value >= 90 ? ' bad' : value >= 75 ? ' warn' : ''}`}>
                <span style={{ width: `${Math.min(100, value)}%` }} />
              </div>
            </div>
          )
        })}
      </div>
    )
  }

  return (
    <ResponsiveContainer width="100%" height={300}>
      <LineChart data={rows}>
        <CartesianGrid strokeDasharray="3 3" stroke="var(--hairline-1)" />
        <XAxis dataKey="time" tick={{ fontSize: 11, fill: 'var(--text-tertiary)' }} stroke="var(--hairline-1)" />
        <YAxis domain={[0, 100]} tick={{ fontSize: 11, fill: 'var(--text-tertiary)' }} stroke="var(--hairline-1)" />
        <Tooltip
          contentStyle={{
            background: 'var(--bg-elevated)',
            border: '1px solid var(--hairline-1)',
            borderRadius: 'var(--radius-md)',
            color: 'var(--text-primary)',
            fontSize: 12,
            boxShadow: 'var(--shadow-2)',
          }}
        />
        <Legend wrapperStyle={{ fontSize: 12, color: 'var(--text-secondary)' }} />
        {keys.map((key, idx) => (
          <Line
            key={key}
            type="monotone"
            dataKey={key}
            stroke={SERIES_COLORS[idx % SERIES_COLORS.length]}
            strokeWidth={2}
            dot={rows.length < 4}
            name={key}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
  )
}
