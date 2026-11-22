// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { useAuth } from '@/lib/auth'
import ZyvorMark from '@/components/ZyvorMark'
import { navFor } from '@/lib/roles'
import { useIsAdmin } from '@/lib/useRole'
import { applyTheme, readStoredTheme, toggleTheme, type Theme } from '@/theme'

interface LayoutProps {
  children: ReactNode
}

/** Same markup and classes as netra's Nav (styled by netra.css), with real links and keyboard support. */
export default function Layout({ children }: LayoutProps) {
  const location = useLocation()
  const navigate = useNavigate()
  const { logout, user } = useAuth()
  const admin = useIsAdmin()
  const navigation = useMemo(() => navFor(admin), [admin])
  const [keyNoticeHidden, setKeyNoticeHidden] = useState(() => {
    try {
      return sessionStorage.getItem('gryvia_default_key_notice') === '1'
    } catch {
      return false
    }
  })
  const [theme, setTheme] = useState<Theme>(readStoredTheme)
  const [openGroup, setOpenGroup] = useState<string | null>(null)
  const navRef = useRef<HTMLElement>(null)
  const mainRef = useRef<HTMLElement>(null)
  const closeTimer = useRef<number | undefined>(undefined)
  const firstRender = useRef(true)

  useEffect(() => {
    applyTheme(theme)
  }, [theme])

  // Move focus to the page on navigation so screen readers announce it.
  useEffect(() => {
    if (firstRender.current) {
      firstRender.current = false
      return
    }
    mainRef.current?.focus({ preventScroll: true })
  }, [location.pathname])

  useEffect(() => {
    const onPointerDown = (e: MouseEvent) => {
      if (navRef.current && !navRef.current.contains(e.target as Node)) setOpenGroup(null)
    }
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpenGroup(null)
    }
    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [])

  const isActive = useCallback(
    (href: string) => (href === '/dashboard' ? location.pathname === '/dashboard' || location.pathname === '/' : location.pathname === href || location.pathname.startsWith(href + '/')),
    [location.pathname],
  )

  const scheduleOpen = (name: string) => {
    window.clearTimeout(closeTimer.current)
    setOpenGroup(name)
  }
  const scheduleClose = () => {
    window.clearTimeout(closeTimer.current)
    closeTimer.current = window.setTimeout(() => setOpenGroup(null), 160)
  }

  const handleLogout = () => {
    logout()
    navigate('/login', { replace: true })
  }

  return (
    <>
      <a
        href="#main"
        className="skip-link"
        onClick={(e) => {
          e.preventDefault()
          mainRef.current?.focus()
        }}
      >
        Skip to content
      </a>
      <nav className="nav" aria-label="Global" ref={navRef}>
        <div className="nav-inner">
          <Link to="/dashboard" className="brand" aria-label="Gryvia home">
            <ZyvorMark className="brand-mark" />
            <span className="brand-word">Gryvia</span>
          </Link>
          <div className="navlinks">
            {navigation.map((item) =>
              'children' in item ? (
                <div key={item.name} className="navgroup" onMouseEnter={() => scheduleOpen(item.name)} onMouseLeave={scheduleClose}>
                  <button
                    type="button"
                    className={item.children.some((c) => isActive(c.href)) ? 'active' : ''}
                    aria-haspopup="true"
                    aria-expanded={openGroup === item.name}
                    onClick={() => setOpenGroup(openGroup === item.name ? null : item.name)}
                  >
                    {item.name}
                  </button>
                  <div className={`mega-panel${openGroup === item.name ? ' open' : ''}`} role="region" aria-label={item.name} onMouseEnter={() => scheduleOpen(item.name)} onMouseLeave={scheduleClose}>
                    <div className="mega-grid">
                      {item.children.map((c) => (
                        <Link key={c.href} to={c.href} className={isActive(c.href) ? 'active' : ''} aria-current={isActive(c.href) ? 'page' : undefined} tabIndex={openGroup === item.name ? 0 : -1} onClick={() => setOpenGroup(null)}>
                          <span className="mega-link-label">{c.name}</span>
                          <span className="mega-link-blurb">{c.blurb}</span>
                        </Link>
                      ))}
                    </div>
                  </div>
                </div>
              ) : (
                <Link key={item.href} to={item.href} className={isActive(item.href) ? 'active' : ''} aria-current={isActive(item.href) ? 'page' : undefined}>
                  {item.name}
                </Link>
              ),
            )}
          </div>
          <div className="nav-actions">
            <button type="button" className="theme-toggle" onClick={handleLogout} aria-label="Log out" title="Log out">
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" aria-hidden>
                <path d="M15 4H7a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h8" strokeLinecap="round" strokeLinejoin="round" />
                <path d="M10 12h11m0 0-3.5-3.5M21 12l-3.5 3.5" strokeLinecap="round" strokeLinejoin="round" />
              </svg>
            </button>
            <button type="button" className="theme-toggle" onClick={() => setTheme(toggleTheme(theme))} aria-label={theme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'} title={theme === 'dark' ? 'Light' : 'Dark'}>
              {theme === 'dark' ? (
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" aria-hidden>
                  <circle cx="12" cy="12" r="4" />
                  <path d="M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41" />
                </svg>
              ) : (
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" aria-hidden>
                  <path d="M21 14.5A8.5 8.5 0 1 1 11.5 3a7 7 0 0 0 9.5 11.5z" />
                </svg>
              )}
            </button>
          </div>
        </div>
      </nav>
      {user?.usingDefaultKey && !keyNoticeHidden && (
        <div className="warning" role="status">
          <span>
            This install uses the default lab key, which is public. Set your own with{' '}
            <code>auth.apiKey</code> (Helm) or <code>GRYVIA_API_KEY</code> before exposing it to anyone else.
          </span>
          <button
            type="button"
            className="btn-secondary"
            onClick={() => {
              setKeyNoticeHidden(true)
              try {
                sessionStorage.setItem('gryvia_default_key_notice', '1')
              } catch {
                /* storage unavailable */
              }
            }}
          >
            Dismiss
          </button>
        </div>
      )}
      <main id="main" ref={mainRef} tabIndex={-1} style={{ outline: 'none' }}>
        <div key={location.pathname}>{children}</div>
      </main>
    </>
  )
}
