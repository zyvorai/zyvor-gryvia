// Pure helpers for the GPU-as-a-Service pages (catalog, usage, tenants).
import { nameError, parseIntStrict } from './forms'

export interface Sku {
  metadata: { name: string; creationTimestamp?: string }
  spec: {
    gpuType: string
    gpusPerUnit?: number
    hourlyRate: number
    currency?: string
    spotDiscount?: number
    description?: string
    enabled?: boolean
  }
}

export interface SkuBody {
  gpuType: string
  gpusPerUnit: number
  hourlyRate: number
  currency: string
  spotDiscount: number
  description: string
  enabled: boolean
}

export interface TenantResource {
  metadata: { name: string; creationTimestamp?: string }
  spec?: {
    displayName?: string
    allowedSkus?: string[]
    namespace?: string
    quotas?: { maxGPUs?: number } & Record<string, unknown>
    networkPolicy?: { isolated?: boolean } & Record<string, unknown>
  }
  status?: { health?: string; memberCount?: number; namespacesManaged?: number; currentUsage?: Record<string, unknown> }
}

export interface CreateTenantBody {
  name: string
  displayName?: string
  allowedSkus?: string[]
  maxGPUs?: number
  isolated: boolean
}

export type UsageGroupBy = 'tenant' | 'sku' | 'day'
export const USAGE_GROUPS: { value: UsageGroupBy; label: string }[] = [
  { value: 'tenant', label: 'Tenant' },
  { value: 'sku', label: 'SKU' },
  { value: 'day', label: 'Day' },
]

export interface UsageRow { key: string; gpuHours: number; cost: number; currency?: string; jobs: number }
export interface FairshareRow {
  tenant: string
  weight: number
  gpuHours: number
  shareTarget: number
  shareActual: number
  recommendedWeight: number
  delta: number
}
export interface FairshareReport {
  lookbackDays: number
  totalGpuHours: number
  applyEnabled: boolean
  items: FairshareRow[]
}
export interface UsageReport {
  groupBy: UsageGroupBy
  items: UsageRow[]
  totals: { gpuHours: number; cost: number; currency?: string; jobs: number }
}

export function parseGroupBy(raw: string | null | undefined, fallback: UsageGroupBy = 'sku'): UsageGroupBy {
  return raw === 'tenant' || raw === 'sku' || raw === 'day' ? raw : fallback
}

/** "$2.50" for USD, "2.5 XYZ" when the currency code is not one Intl knows. */
export function formatRate(rate?: number | null, currency = 'USD'): string {
  if (rate === undefined || rate === null || !Number.isFinite(rate)) return '—'
  try {
    return new Intl.NumberFormat('en-US', { style: 'currency', currency, minimumFractionDigits: 2, maximumFractionDigits: 4 }).format(rate)
  } catch {
    return `${rate} ${currency}`
  }
}

/** Hourly rate after the spot discount (percent 0-100). */
export function spotRate(rate: number, discountPercent?: number): number {
  const d = Math.min(100, Math.max(0, discountPercent ?? 0))
  return Number(((rate * (100 - d)) / 100).toFixed(6))
}

/** Tenants only see SKUs an admin has enabled; admins see all of them. */
export function visibleSkus(skus: Sku[], admin: boolean): Sku[] {
  return admin ? skus : skus.filter((s) => s.spec.enabled !== false)
}

export interface SkuForm {
  name: string
  gpuType: string
  gpusPerUnit: string
  hourlyRate: string
  currency: string
  spotDiscount: string
  description: string
  enabled: boolean
}

export const EMPTY_SKU_FORM: SkuForm = { name: '', gpuType: '', gpusPerUnit: '1', hourlyRate: '', currency: 'USD', spotDiscount: '0', description: '', enabled: true }

export function skuToForm(s: Sku): SkuForm {
  return {
    name: s.metadata.name,
    gpuType: s.spec.gpuType,
    gpusPerUnit: String(s.spec.gpusPerUnit ?? 1),
    hourlyRate: String(s.spec.hourlyRate),
    currency: s.spec.currency ?? 'USD',
    spotDiscount: String(s.spec.spotDiscount ?? 0),
    description: s.spec.description ?? '',
    enabled: s.spec.enabled !== false,
  }
}

/** Strict non-negative decimal ("2.5", "10"); null when blank or junk. */
export function parseRate(raw: string): number | null {
  const t = raw.trim()
  return /^\d+(\.\d+)?$/.test(t) ? Number(t) : null
}

export type SkuErrors = Partial<Record<'name' | 'gpuType' | 'gpusPerUnit' | 'hourlyRate' | 'currency' | 'spotDiscount', string>>

export function skuErrors(f: SkuForm, editing: boolean): SkuErrors {
  const e: SkuErrors = {}
  if (!editing) {
    const n = nameError(f.name)
    if (n) e.name = n
  }
  if (f.gpuType.trim() === '') e.gpuType = 'Enter the GPU type, for example A100-80G.'
  const g = parseIntStrict(f.gpusPerUnit)
  if (g === null || g < 1 || g > 64) e.gpusPerUnit = 'Enter a whole number from 1 to 64.'
  if (parseRate(f.hourlyRate) === null) e.hourlyRate = 'Enter the hourly rate as a number, for example 2.50.'
  if (!/^[A-Za-z]{3}$/.test(f.currency.trim())) e.currency = 'Use a 3-letter currency code, for example USD.'
  const d = parseIntStrict(f.spotDiscount)
  if (d === null || d < 0 || d > 100) e.spotDiscount = 'Enter a whole percent from 0 to 100.'
  return e
}

/** The request body for POST (with name) and PUT (the caller drops name). Only call when skuErrors is empty. */
export function buildSkuBody(f: SkuForm): SkuBody & { name: string } {
  return {
    name: f.name.trim(),
    gpuType: f.gpuType.trim(),
    gpusPerUnit: parseIntStrict(f.gpusPerUnit) ?? 1,
    hourlyRate: parseRate(f.hourlyRate) ?? 0,
    currency: f.currency.trim().toUpperCase(),
    spotDiscount: parseIntStrict(f.spotDiscount) ?? 0,
    description: f.description.trim(),
    enabled: f.enabled,
  }
}

export interface TenantForm { name: string; displayName: string; allowedSkus: string[]; maxGPUs: string; isolated: boolean }
export const EMPTY_TENANT_FORM: TenantForm = { name: '', displayName: '', allowedSkus: [], maxGPUs: '', isolated: true }

export function tenantErrors(f: TenantForm): Partial<Record<'name' | 'maxGPUs', string>> {
  const e: Partial<Record<'name' | 'maxGPUs', string>> = {}
  const n = nameError(f.name)
  if (n) e.name = n
  if (f.maxGPUs.trim() !== '') {
    const m = parseIntStrict(f.maxGPUs)
    if (m === null || m < 0 || m > 100000) e.maxGPUs = 'Enter a whole number from 0 to 100000, or leave blank.'
  }
  return e
}

/** Blank optional fields are left out so the gateway applies its defaults. */
export function buildTenantBody(f: TenantForm): CreateTenantBody {
  const body: CreateTenantBody = { name: f.name.trim(), isolated: f.isolated }
  if (f.displayName.trim()) body.displayName = f.displayName.trim()
  if (f.allowedSkus.length) body.allowedSkus = [...f.allowedSkus]
  const max = parseIntStrict(f.maxGPUs)
  if (f.maxGPUs.trim() !== '' && max !== null) body.maxGPUs = max
  return body
}

export function toggleValue(list: string[], value: string): string[] {
  return list.includes(value) ? list.filter((v) => v !== value) : [...list, value]
}

const pad = (n: number) => String(n).padStart(2, '0')
export const isoDate = (d: Date) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`

/** The last `days` days up to and including today, as YYYY-MM-DD in local time. */
export function defaultUsageRange(now: Date = new Date(), days = 30): { from: string; to: string } {
  const from = new Date(now.getFullYear(), now.getMonth(), now.getDate() - (days - 1))
  return { from: isoDate(from), to: isoDate(now) }
}

export function rangeError(from: string, to: string): string | null {
  if (from && to && from > to) return 'The start date is after the end date.'
  return null
}

export interface UsageFilters { from?: string; to?: string; groupBy: UsageGroupBy; tenant?: string }

/** Query params for /usage and /usage/export; blank values are dropped. */
export function usageParams(f: UsageFilters, extra: Record<string, string> = {}): Record<string, string> {
  const out: Record<string, string> = { ...extra }
  if (f.tenant) out.tenant = f.tenant
  if (f.from) out.from = f.from
  if (f.to) out.to = f.to
  return out
}

export function exportFilename(format: 'csv' | 'json', f: { from?: string; to?: string; tenant?: string }): string {
  const parts = ['gryvia-usage', f.tenant, f.from, f.to].filter((p): p is string => !!p)
  return `${parts.join('-')}.${format}`
}

/** Header labels for the usage key column. */
export function keyHeader(groupBy: UsageGroupBy): string {
  return groupBy === 'tenant' ? 'Tenant' : groupBy === 'sku' ? 'SKU' : 'Day'
}
