// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import Modal from './Modal'
import ConfirmDialog from './ConfirmDialog'

function Harness({ dirty = false }: { dirty?: boolean }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button onClick={() => setOpen(true)}>open</button>
      {open && (
        <Modal title="Create thing" onClose={() => setOpen(false)} dirty={dirty}>
          <input aria-label="name" />
          <button>submit</button>
        </Modal>
      )}
    </>
  )
}

describe('Modal', () => {
  it('is an accessible dialog and focuses the first field', async () => {
    render(<Harness />)
    await userEvent.click(screen.getByText('open'))
    const dialog = screen.getByRole('dialog', { name: 'Create thing' })
    expect(dialog).toHaveAttribute('aria-modal', 'true')
    expect(screen.getByLabelText('name')).toHaveFocus()
    expect(document.body.style.overflow).toBe('hidden')
  })

  it('closes on Escape, restores focus to the opener and unlocks scroll', async () => {
    render(<Harness />)
    const opener = screen.getByText('open')
    await userEvent.click(opener)
    await userEvent.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(opener).toHaveFocus()
    expect(document.body.style.overflow).not.toBe('hidden')
  })

  it('traps Tab inside the dialog', async () => {
    render(<Harness />)
    await userEvent.click(screen.getByText('open'))
    const submit = screen.getByText('submit')
    submit.focus()
    await userEvent.tab()
    expect(screen.getByLabelText('name')).toHaveFocus()
    await userEvent.tab({ shift: true })
    expect(submit).toHaveFocus()
  })

  it('closes on backdrop click, but not when the form is dirty', async () => {
    const { unmount } = render(<Harness />)
    await userEvent.click(screen.getByText('open'))
    fireEvent.mouseDown(screen.getByRole('dialog').parentElement!)
    expect(screen.queryByRole('dialog')).toBeNull()
    unmount()

    render(<Harness dirty />)
    await userEvent.click(screen.getByText('open'))
    fireEvent.mouseDown(screen.getByRole('dialog').parentElement!)
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    await userEvent.keyboard('{Escape}')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })
})

describe('ConfirmDialog', () => {
  it('names the consequence, starts on Cancel for danger, and confirms', async () => {
    const onConfirm = vi.fn()
    const onCancel = vi.fn()
    render(
      <ConfirmDialog title="Delete workspace?" confirmLabel="Delete" onConfirm={onConfirm} onCancel={onCancel}>
        Deleting ws-1 also deletes its volume.
      </ConfirmDialog>,
    )
    expect(screen.getByText(/also deletes its volume/)).toBeInTheDocument()
    expect(screen.getByText('Cancel')).toHaveFocus()
    await userEvent.click(screen.getByText('Delete'))
    expect(onConfirm).toHaveBeenCalledTimes(1)
    await userEvent.keyboard('{Escape}')
    expect(onCancel).toHaveBeenCalled()
  })
})
