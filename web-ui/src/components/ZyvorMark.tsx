// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

/** zyvor.dev brand: the open orange "Z" stroke, and the "Z zyvor" lockup (same as Zorvia's). */

export const ZYVOR_URL = 'https://zyvor.dev'
export const ZYVOR_ACCENT = '#ff5a15'

export default function ZyvorMark({ className = 'zyvor-mark', title }: { className?: string; title?: string }) {
  return (
    <svg
      viewBox="0 0 18 18"
      className={className}
      aria-hidden={title ? undefined : true}
      role={title ? 'img' : undefined}
    >
      {title ? <title>{title}</title> : null}
      <path
        d="M2 2h14L6.6 16H16"
        fill="none"
        stroke={ZYVOR_ACCENT}
        strokeWidth="2.6"
        strokeLinejoin="round"
        strokeLinecap="round"
      />
    </svg>
  )
}

export function ZyvorLockup({ markClassName, showWordmark = true }: { markClassName?: string; showWordmark?: boolean }) {
  return (
    <span className="zyvor-lockup">
      <ZyvorMark className={markClassName} />
      {showWordmark ? <span className="zyvor-wordmark">zyvor</span> : null}
    </span>
  )
}
