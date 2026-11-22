// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import type { ReactNode } from 'react'
import { AlertCircle, CheckCircle2 } from 'lucide-react'
import '@/styles/zyvor-login.css'

/** Zorvia's chapter login (PremiumLoginShell): a hero, what the product does, then the credentials chapter. */

export type LoginTone = 'sky' | 'violet' | 'emerald' | 'amber' | 'orange' | 'teal'

export interface LoginFeature {
  icon: ReactNode
  title: string
  text: string
  tone: LoginTone
}

export interface LoginAbout {
  eyebrow: string
  title: ReactNode
  lede: ReactNode
  features: LoginFeature[]
  goalsTitle: string
  goals: string[]
}

export interface LoginShellProps {
  brand: ReactNode
  wordmark: string
  instanceMeta?: ReactNode
  heroTitle: ReactNode
  heroLede: ReactNode
  pills: { label: string; tone: LoginTone }[]
  heroCta?: ReactNode
  heroSecondary?: ReactNode
  chapterNote?: ReactNode
  about?: LoginAbout
  formHeading: ReactNode
  hint?: ReactNode
  footer?: ReactNode
  children: ReactNode
}

export function LoginShell(props: LoginShellProps) {
  return (
    <div className="login-page" data-tone="orange">
      <main className="login-scroll" aria-label="Sign in">
        <section className="login-chapter login-chapter-hero" aria-label={props.wordmark}>
          <div className="login-mesh" aria-hidden>
            <span className="login-blob login-blob-a" />
            <span className="login-blob login-blob-b" />
            <span className="login-blob login-blob-c" />
            <span className="login-blob login-blob-d" />
          </div>
          <div className="login-chapter-inner">
            {props.brand}
            <p className="login-wordmark">{props.wordmark}</p>
            {props.instanceMeta ? <div className="login-instance-meta">{props.instanceMeta}</div> : null}
            <h1 className="login-hero-title">{props.heroTitle}</h1>
            <p className="login-tagline">{props.heroLede}</p>
            <div className="login-pill-row">
              {props.pills.map((pill) => (
                <span key={pill.label} data-tone={pill.tone} className="login-pill">
                  <span className="login-pill-dot" aria-hidden />
                  {pill.label}
                </span>
              ))}
            </div>
            {props.heroCta || props.heroSecondary ? (
              <div className="login-cta">
                {props.heroCta}
                {props.heroSecondary}
              </div>
            ) : null}
            {props.chapterNote ? <p className="login-chapter-note">{props.chapterNote}</p> : null}
          </div>
        </section>
        {props.about ? (
          <section id="login-about" className="login-chapter login-chapter-about" aria-label={props.about.eyebrow}>
            <div className="login-chapter-inner login-about-inner">
              <p className="login-eyebrow">{props.about.eyebrow}</p>
              <h2 className="login-about-title">{props.about.title}</h2>
              <p className="login-tagline">{props.about.lede}</p>
              <ul className="login-features">
                {props.about.features.map((f) => (
                  <li key={f.title} className="login-feature" data-tone={f.tone}>
                    <span className="login-feature-icon" aria-hidden>
                      {f.icon}
                    </span>
                    <h3>{f.title}</h3>
                    <p>{f.text}</p>
                  </li>
                ))}
              </ul>
              <div className="login-goals">
                <h3>{props.about.goalsTitle}</h3>
                <ul>
                  {props.about.goals.map((g) => (
                    <li key={g}>
                      <CheckCircle2 size={18} aria-hidden />
                      {g}
                    </li>
                  ))}
                </ul>
              </div>
            </div>
          </section>
        ) : null}
        <section id="login-sign-in" className="login-chapter login-chapter-sign-in" aria-label="Credentials">
          <div className="login-chapter-inner login-sign-in-inner">
            <h2 className="login-form-heading">{props.formHeading}</h2>
            <div className="login-panel">{props.children}</div>
            {props.hint ? <div className="login-hint">{props.hint}</div> : null}
          </div>
        </section>
        {props.footer}
      </main>
    </div>
  )
}

export function LoginError({ id, message }: { id?: string; message: string }) {
  return (
    <div id={id} className="login-alert" role="alert" aria-live="assertive">
      <AlertCircle size={16} aria-hidden />
      <div>
        <p className="login-alert-title">Unable to sign in</p>
        <p className="login-alert-text">{message}</p>
      </div>
    </div>
  )
}

export function LoginField({ label, id, children }: { label: string; id: string; children: ReactNode }) {
  return (
    <div className="login-field">
      <label htmlFor={id} className="login-field-label">
        {label}
      </label>
      <div className="login-field-control">{children}</div>
    </div>
  )
}
