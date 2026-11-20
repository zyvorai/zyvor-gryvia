// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useEffect, useId, useRef, type ReactNode } from 'react'
import { createPortal } from 'react-dom'

const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'

type ModalProps = {
  title: string
  onClose: () => void
  children: ReactNode
  /** Unsaved input: a stray backdrop click or Escape will not discard it. */
  dirty?: boolean
  /** Extra className for the card (e.g. a wider card). */
  className?: string
}

/**
 * Accessible dialog: role/aria-modal/labelledby, focus moves in and is trapped, Escape closes,
 * focus returns to whatever opened it, and the page behind does not scroll.
 */
export default function Modal({ title, onClose, children, dirty = false, className }: ModalProps) {
  const titleId = useId()
  const cardRef = useRef<HTMLDivElement>(null)
  const onCloseRef = useRef(onClose)
  const dirtyRef = useRef(dirty)
  // Keep the latest callbacks/flags without re-running the focus/keyboard effect.
  useEffect(() => {
    onCloseRef.current = onClose
    dirtyRef.current = dirty
  })

  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null
    const previousOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'

    const card = cardRef.current
    const first = card?.querySelector<HTMLElement>('[data-autofocus], input, select, textarea') ?? card?.querySelector<HTMLElement>(FOCUSABLE)
    ;(first ?? card)?.focus()

    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !dirtyRef.current) {
        e.stopPropagation()
        onCloseRef.current()
        return
      }
      if (e.key !== 'Tab' || !card) return
      const items = Array.from(card.querySelectorAll<HTMLElement>(FOCUSABLE))
      if (items.length === 0) {
        e.preventDefault()
        return
      }
      const firstItem = items[0]
      const lastItem = items[items.length - 1]
      const active = document.activeElement
      if (e.shiftKey && (active === firstItem || active === card)) {
        e.preventDefault()
        lastItem.focus()
      } else if (!e.shiftKey && active === lastItem) {
        e.preventDefault()
        firstItem.focus()
      }
    }
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      document.body.style.overflow = previousOverflow
      opener?.focus?.()
    }
  }, [])

  return createPortal(
    <div
      className="modal-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget && !dirtyRef.current) onCloseRef.current()
      }}
    >
      <div
        ref={cardRef}
        className={`modal-card${className ? ' ' + className : ''}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
      >
        <h2 id={titleId} className="card-title">
          {title}
        </h2>
        {children}
      </div>
    </div>,
    document.body,
  )
}
