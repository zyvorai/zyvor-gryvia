// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import toast from 'react-hot-toast'
import { errorMessage } from './errors'

/** One place for user feedback after an action. */
export const notify = {
  success: (message: string) => toast.success(message),
  info: (message: string) => toast(message),
  /** Shows what went wrong, using the gateway's reason when there is one. */
  error: (what: string, err?: unknown) => toast.error(err === undefined ? what : `${what}: ${errorMessage(err)}`),
}
