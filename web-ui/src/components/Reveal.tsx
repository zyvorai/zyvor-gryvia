// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useEffect, useRef, useState } from 'react'
import type { ReactNode } from 'react'

type RevealProps = {
  children: ReactNode
  className?: string
  /** Stagger delay in ms, for revealing a sequence of siblings. */
  delay?: number
}

/** Fades a section in once it scrolls into view; always visible without IntersectionObserver or with reduced motion (CSS). */
export default function Reveal({ children, className, delay = 0 }: RevealProps) {
  const ref = useRef<HTMLDivElement>(null)
  const [visible, setVisible] = useState(false)

  useEffect(() => {
    const node = ref.current
    if (!node || typeof IntersectionObserver === 'undefined') {
      setVisible(true)
      return
    }
    const observer = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) {
          setVisible(true)
          observer.disconnect()
        }
      },
      { threshold: 0.15 },
    )
    observer.observe(node)
    return () => observer.disconnect()
  }, [])

  const cls = ['zv-reveal', visible && 'zv-reveal--visible', className].filter(Boolean).join(' ')
  return (
    <div ref={ref} className={cls} style={delay ? { transitionDelay: `${delay}ms` } : undefined}>
      {children}
    </div>
  )
}
