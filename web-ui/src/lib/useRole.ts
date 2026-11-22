// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useAuth } from './auth'
import { isAdminUser } from './roles'

/** True for admins (and for old gateways that report no role). */
export function useIsAdmin(): boolean {
  return isAdminUser(useAuth().user)
}
