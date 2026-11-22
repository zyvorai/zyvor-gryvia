// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { describe, expect, it } from 'vitest'
import {
  EMPTY_SKU_FORM, EMPTY_TENANT_FORM, buildSkuBody, buildTenantBody, defaultUsageRange, exportFilename, formatRate, keyHeader, parseGroupBy,
  parseRate, rangeError, skuErrors, skuToForm, spotRate, tenantErrors, toggleValue, usageParams, visibleSkus, type Sku,
} from './cloud'

const sku = (name: string, enabled?: boolean): Sku => ({ metadata: { name }, spec: { gpuType: 'A100', hourlyRate: 2.5, enabled } })

describe('formatRate / spotRate', () => {
  it('formats currency and falls back for unknown codes', () => {
    expect(formatRate(2.5, 'USD')).toBe('$2.50')
    expect(formatRate(0.0125)).toBe('$0.0125')
    expect(formatRate(undefined)).toBe('—')
    expect(formatRate(3, 'XX')).toBe('3 XX')
  })
  it('applies the spot discount', () => {
    expect(spotRate(10, 30)).toBe(7)
    expect(spotRate(10)).toBe(10)
    expect(spotRate(10, 150)).toBe(0)
  })
})

describe('visibleSkus', () => {
  it('hides disabled SKUs from tenants only', () => {
    const list = [sku('a'), sku('b', false), sku('c', true)]
    expect(visibleSkus(list, false).map((s) => s.metadata.name)).toEqual(['a', 'c'])
    expect(visibleSkus(list, true)).toHaveLength(3)
  })
})

describe('sku form', () => {
  const valid = { ...EMPTY_SKU_FORM, name: 'a100', gpuType: 'A100-80G', hourlyRate: '2.50' }
  it('validates every field', () => {
    expect(skuErrors(valid, false)).toEqual({})
    const e = skuErrors({ ...EMPTY_SKU_FORM, gpusPerUnit: '0', currency: 'DOLLARS', spotDiscount: '101' }, false)
    expect(Object.keys(e).sort()).toEqual(['currency', 'gpuType', 'gpusPerUnit', 'hourlyRate', 'name', 'spotDiscount'])
  })
  it('does not require a name when editing', () => {
    expect(skuErrors({ ...valid, name: '' }, true)).toEqual({})
  })
  it('builds the request body', () => {
    expect(buildSkuBody({ ...valid, currency: 'eur', spotDiscount: '30', description: ' 80GB ' })).toEqual({
      name: 'a100', gpuType: 'A100-80G', gpusPerUnit: 1, hourlyRate: 2.5, currency: 'EUR', spotDiscount: 30, description: '80GB', enabled: true,
    })
  })
  it('round-trips through the form', () => {
    const s: Sku = { metadata: { name: 'x' }, spec: { gpuType: 'H100', gpusPerUnit: 8, hourlyRate: 30, currency: 'USD', spotDiscount: 40, description: 'd', enabled: false } }
    expect(skuToForm(s)).toEqual({ name: 'x', gpuType: 'H100', gpusPerUnit: '8', hourlyRate: '30', currency: 'USD', spotDiscount: '40', description: 'd', enabled: false })
  })
  it('parses rates strictly', () => {
    expect(parseRate('2.5')).toBe(2.5)
    for (const bad of ['', '-1', '1e3', 'abc', '1,5']) expect(parseRate(bad)).toBeNull()
  })
})

describe('tenant form', () => {
  it('validates name and optional max GPUs', () => {
    expect(tenantErrors({ ...EMPTY_TENANT_FORM, name: 'acme' })).toEqual({})
    expect(tenantErrors({ ...EMPTY_TENANT_FORM, name: 'Acme' }).name).toBeTruthy()
    expect(tenantErrors({ ...EMPTY_TENANT_FORM, name: 'acme', maxGPUs: 'x' }).maxGPUs).toBeTruthy()
  })
  it('omits blank optional fields', () => {
    expect(buildTenantBody({ ...EMPTY_TENANT_FORM, name: ' acme ' })).toEqual({ name: 'acme', isolated: true })
    expect(buildTenantBody({ name: 'acme', displayName: 'Acme', allowedSkus: ['a'], maxGPUs: '8', isolated: false })).toEqual({ name: 'acme', displayName: 'Acme', allowedSkus: ['a'], maxGPUs: 8, isolated: false })
  })
  it('toggles list membership', () => {
    expect(toggleValue(['a'], 'b')).toEqual(['a', 'b'])
    expect(toggleValue(['a', 'b'], 'a')).toEqual(['b'])
  })
})

describe('usage helpers', () => {
  it('defaults to the last 30 days in local time', () => {
    expect(defaultUsageRange(new Date(2026, 2, 1))).toEqual({ from: '2026-01-31', to: '2026-03-01' })
  })
  it('flags a reversed range', () => {
    expect(rangeError('2026-02-01', '2026-01-01')).toBeTruthy()
    expect(rangeError('2026-01-01', '2026-01-01')).toBeNull()
    expect(rangeError('', '2026-01-01')).toBeNull()
  })
  it('drops blank params', () => {
    expect(usageParams({ groupBy: 'sku', from: '2026-01-01', to: '', tenant: '' }, { groupBy: 'sku' })).toEqual({ groupBy: 'sku', from: '2026-01-01' })
  })
  it('parses the grouping with a fallback', () => {
    expect(parseGroupBy('day')).toBe('day')
    expect(parseGroupBy('nope')).toBe('sku')
    expect(parseGroupBy(null, 'tenant')).toBe('tenant')
    expect(keyHeader('tenant')).toBe('Tenant')
  })
  it('names export files', () => {
    expect(exportFilename('csv', { from: '2026-01-01', to: '2026-01-31', tenant: 'acme' })).toBe('gryvia-usage-acme-2026-01-01-2026-01-31.csv')
    expect(exportFilename('json', {})).toBe('gryvia-usage.json')
  })
})
