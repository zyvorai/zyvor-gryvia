// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import StepEditor from './StepEditor'
import { emptyStep, initialSpecState, type SpecState, type StepDraft } from '@/lib/workflowSteps'

const job = (name: string, dependsOn: string[] = []): StepDraft => emptyStep({ name, dependsOn, image: 'img:1' })

/** Holds the editor's state like the create dialog does and exposes the latest value. */
function renderEditor(steps: StepDraft[], showErrors = false) {
  const latest: { current: SpecState } = { current: initialSpecState(steps) }
  function Harness() {
    const [state, setState] = useState(latest.current)
    latest.current = state
    return <StepEditor state={state} onChange={setState} showErrors={showErrors} />
  }
  render(<Harness />)
  return latest
}

const names = (latest: { current: SpecState }) => latest.current.steps.map((s) => s.name)
const stepBox = (n: number) => screen.getByRole('group', { name: new RegExp(`^Step ${n}:`) })
const nameInput = (n: number) => within(stepBox(n)).getByRole('textbox', { name: /^Name/ })

describe('StepEditor', () => {
  it('adds a step with a unique name and focuses it', async () => {
    const latest = renderEditor([job('a')])
    await userEvent.click(screen.getByRole('button', { name: 'Add step' }))
    expect(names(latest)).toEqual(['a', 'step-2'])
    expect(nameInput(2)).toHaveFocus()
  })

  it('removes a step and drops dependencies on it', async () => {
    const latest = renderEditor([job('a'), job('b', ['a'])])
    await userEvent.click(screen.getByRole('button', { name: 'Remove step a' }))
    expect(names(latest)).toEqual(['b'])
    expect(latest.current.steps[0].dependsOn).toEqual([])
  })

  it('reorders steps with the move buttons and disables them at the ends', async () => {
    const latest = renderEditor([job('a'), job('b'), job('c')])
    expect(screen.getByRole('button', { name: 'Move up: step a' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Move down: step c' })).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: 'Move down: step a' }))
    expect(names(latest)).toEqual(['b', 'a', 'c'])
    await userEvent.click(screen.getByRole('button', { name: 'Move up: step c' }))
    expect(names(latest)).toEqual(['b', 'c', 'a'])
  })

  it('keeps dependents attached when a step is renamed, even through a blank name', async () => {
    const latest = renderEditor([job('a'), job('b', ['a'])])
    const input = nameInput(1)
    await userEvent.clear(input)
    expect(latest.current.steps[1].dependsOn).toEqual(['a'])
    await userEvent.type(input, 'prep')
    expect(names(latest)).toEqual(['prep', 'b'])
    expect(latest.current.steps[1].dependsOn).toEqual(['prep'])
  })

  it('drops dependents of a blanked step when it is removed', async () => {
    const latest = renderEditor([job('a'), job('b', ['a'])])
    await userEvent.clear(nameInput(1))
    await userEvent.click(screen.getByRole('button', { name: 'Remove step step 1' }))
    expect(latest.current.steps[0].dependsOn).toEqual([])
  })

  it('toggles a dependency with its checkbox', async () => {
    const latest = renderEditor([job('a'), job('b')])
    await userEvent.click(within(stepBox(2)).getByRole('checkbox', { name: 'a' }))
    expect(latest.current.steps[1].dependsOn).toEqual(['a'])
    await userEvent.click(within(stepBox(2)).getByRole('checkbox', { name: 'a' }))
    expect(latest.current.steps[1].dependsOn).toEqual([])
  })

  it('shows a cycle error as an alert', () => {
    renderEditor([job('a', ['b']), job('b', ['a'])], true)
    expect(screen.getByRole('alert')).toHaveTextContent(/cycle/i)
  })

  it('marks a bad field invalid and describes it by its message', async () => {
    renderEditor([job('a')])
    const input = nameInput(1)
    expect(input).toHaveAttribute('aria-invalid', 'false')
    await userEvent.clear(input)
    await userEvent.type(input, 'Bad Name')
    expect(input).toHaveAttribute('aria-invalid', 'true')
    const msg = document.getElementById(input.getAttribute('aria-describedby')!)
    expect(msg).toHaveTextContent(/lowercase/i)
  })

  it('describes the execution order as text for screen readers', () => {
    renderEditor([job('b', ['a']), job('a')])
    const list = screen.getByRole('list', { name: 'Execution order' })
    const items = within(list).getAllByRole('listitem').map((li) => li.textContent)
    expect(items).toEqual(['a: stage 1, no dependencies, type job', 'b: stage 2, runs after a, type job'])
    expect(screen.getByRole('img', { name: /diagram of the steps/i })).toBeInTheDocument()
  })

  it('switches to the JSON view and back', async () => {
    renderEditor([job('a')])
    await userEvent.click(screen.getByRole('button', { name: /edit as JSON/ }))
    expect(screen.getByRole('textbox', { name: /Workflow spec/ })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Back to the step editor' }))
    expect(nameInput(1)).toHaveValue('a')
  })
})
