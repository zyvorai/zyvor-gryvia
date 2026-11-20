// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-1.0

import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import ParamSpaceEditor from './ParamSpaceEditor'
import { emptyRow, initialSpaceState, type ParamRow, type ParamSpaceState } from '@/lib/paramSpace'

function renderEditor(rows: ParamRow[], showErrors = false) {
  const latest: { current: ParamSpaceState } = { current: initialSpaceState(rows) }
  function Harness() {
    const [state, setState] = useState(latest.current)
    latest.current = state
    return <ParamSpaceEditor state={state} onChange={setState} showErrors={showErrors} />
  }
  render(<Harness />)
  return latest
}

const choice = (values: string[] = []) => emptyRow({ name: 'batch', type: 'choice', values })
const box = (n: number) => screen.getByRole('group', { name: new RegExp(`^Parameter ${n}`) })

describe('ParamSpaceEditor', () => {
  it('adds and removes parameter rows', async () => {
    const latest = renderEditor([choice(['16'])])
    await userEvent.click(screen.getByRole('button', { name: 'Add parameter' }))
    expect(latest.current.rows).toHaveLength(2)
    await userEvent.click(screen.getByRole('button', { name: 'Remove parameter 1 batch' }))
    expect(latest.current.rows).toHaveLength(1)
  })

  it('adds choice values as chips on Enter and comma, ignoring duplicates', async () => {
    const latest = renderEditor([choice()])
    const input = within(box(1)).getByRole('textbox', { name: 'Values' })
    await userEvent.type(input, '16{Enter}')
    await userEvent.type(input, '32,64,')
    await userEvent.type(input, '16{Enter}')
    expect(latest.current.rows[0].values).toEqual(['16', '32', '64'])
    const chips = within(screen.getByRole('list', { name: 'Values added' })).getAllByRole('listitem')
    expect(chips).toHaveLength(3)
  })

  it('removes a chip with its button and with Backspace on an empty input', async () => {
    const latest = renderEditor([choice(['16', '32', '64'])])
    await userEvent.click(screen.getByRole('button', { name: 'Remove value 32' }))
    expect(latest.current.rows[0].values).toEqual(['16', '64'])
    await userEvent.type(within(box(1)).getByRole('textbox', { name: 'Values' }), '{Backspace}')
    expect(latest.current.rows[0].values).toEqual(['16'])
  })

  it('does not submit a surrounding form when Enter adds a chip', async () => {
    let submitted = false
    const latest: { current: ParamSpaceState } = { current: initialSpaceState([choice()]) }
    function Harness() {
      const [state, setState] = useState(latest.current)
      return (
        <form onSubmit={(e) => { e.preventDefault(); submitted = true }}>
          <ParamSpaceEditor state={state} onChange={setState} />
        </form>
      )
    }
    render(<Harness />)
    await userEvent.type(screen.getByRole('textbox', { name: 'Values' }), '8{Enter}')
    expect(submitted).toBe(false)
  })

  it('marks bad numeric input invalid and describes it', async () => {
    renderEditor([emptyRow({ name: 'lr', type: 'float', min: '0.1', max: '0.5' })])
    const max = within(box(1)).getByRole('textbox', { name: /^Max/ })
    expect(max).toHaveAttribute('aria-invalid', 'false')
    await userEvent.clear(max)
    await userEvent.type(max, 'abc')
    expect(max).toHaveAttribute('aria-invalid', 'true')
    expect(document.getElementById(max.getAttribute('aria-describedby')!)).toHaveTextContent('Enter a number.')
  })

  it('marks an empty choice list invalid once errors are shown', () => {
    renderEditor([choice()], true)
    const values = screen.getByRole('textbox', { name: 'Values' })
    expect(values).toHaveAttribute('aria-invalid', 'true')
    expect(document.getElementById(values.getAttribute('aria-describedby')!)).toHaveTextContent('Add at least one value.')
  })

  it('keeps the JSON view in sync with the rows', async () => {
    renderEditor([choice(['16'])])
    await userEvent.click(screen.getByRole('button', { name: /edit as JSON/ }))
    const json = screen.getByRole('textbox', { name: /Parameter space \(JSON\)/ })
    expect((json as HTMLTextAreaElement).value).toContain('"batch"')
  })
})
