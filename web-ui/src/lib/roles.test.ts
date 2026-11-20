// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { describe, expect, it } from 'vitest'
import { ADMIN_ONLY_PREFIXES, NAVIGATION, TENANT_HOME, canAccessPath, isAdminUser, navFor } from './roles'

const hrefs = (admin: boolean) => navFor(admin).flatMap((i) => ('children' in i ? i.children.map((c) => c.href) : [i.href]))
const groups = (admin: boolean) => navFor(admin).map((i) => i.name)

describe('isAdminUser', () => {
  it('is admin for role admin, a missing role and no user', () => {
    expect(isAdminUser({ role: 'admin' })).toBe(true)
    expect(isAdminUser({})).toBe(true)
    expect(isAdminUser(null)).toBe(true)
  })
  it('is not admin for tenants', () => {
    expect(isAdminUser({ role: 'tenant' })).toBe(false)
  })
})

describe('navFor', () => {
  it('shows everything to admins', () => {
    expect(groups(true)).toEqual(['Dashboard', 'Work', 'Models', 'Cloud', 'Platform', 'Observe'])
    expect(hrefs(true)).toEqual(expect.arrayContaining(['/catalog', '/usage', '/invoices', '/tenants', '/sovereign', '/nodes', '/quotas', '/costs', '/gpu', '/network', '/security']))
  })
  it('hides admin-only groups from tenants but keeps the cloud pages', () => {
    expect(groups(false)).toEqual(['Dashboard', 'Work', 'Models', 'Cloud'])
    const h = hrefs(false)
    for (const p of ADMIN_ONLY_PREFIXES) expect(h.some((x) => x.startsWith(p))).toBe(false)
    expect(h).toEqual(expect.arrayContaining(['/catalog', '/usage', '/invoices', '/jobs']))
  })
  it('drops admin-only leaves and empties out groups with none left', () => {
    const nav = [{ name: 'G', children: [{ name: 'A', href: '/a', blurb: '', adminOnly: true }] }, ...NAVIGATION.slice(0, 1)]
    expect(navFor(false, nav)).toEqual(NAVIGATION.slice(0, 1))
    expect(navFor(true, nav)).toHaveLength(2)
  })
})

describe('canAccessPath', () => {
  it('lets admins open everything', () => {
    for (const p of ['/nodes', '/quotas/x', '/network/flows', '/security', '/gpu/communication', '/costs']) expect(canAccessPath(p, true)).toBe(true)
  })
  it('blocks tenants from admin pages and their subpaths only', () => {
    for (const p of ['/sovereign', '/nodes', '/quotas', '/network', '/network/flows', '/gpu/communication', '/security', '/audit', '/costs']) expect(canAccessPath(p, false)).toBe(false)
    for (const p of ['/catalog', '/usage', '/invoices', '/tenants', '/jobs', '/jobs/new', '/networking-docs', '/dashboard']) expect(canAccessPath(p, false)).toBe(true)
  })
  it('sends tenants to the catalog', () => {
    expect(TENANT_HOME).toBe('/catalog')
  })
})
