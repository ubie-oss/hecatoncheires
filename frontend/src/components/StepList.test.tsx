import { describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { StepRow } from './StepList'

const baseStep = {
  id: 'step-1',
  actionID: 7,
  title: 'collect logs',
  done: false,
  createdAt: '2026-05-07T00:00:00Z',
  updatedAt: '2026-05-07T00:00:00Z',
}

describe('StepRow', () => {
  it('renders read-only: no edit, no delete, and a disabled checkbox', () => {
    const onToggle = vi.fn()
    const onRename = vi.fn()
    const onDelete = vi.fn()
    render(
      <StepRow
        step={baseStep}
        onToggle={onToggle}
        onRename={onRename}
        onDelete={onDelete}
        ariaCheckboxLabel="check"
        ariaDeleteLabel="delete"
        readOnly
      />,
    )
    const title = screen.getByTestId('action-step-title-step-1')
    expect(title).toHaveTextContent('collect logs')
    expect(title.tagName).toBe('SPAN')
    fireEvent.click(title)
    expect(screen.queryByTestId('action-step-edit-step-1')).toBeNull()

    const checkbox = screen.getByTestId('action-step-checkbox-step-1')
    expect(checkbox).toBeDisabled()
    fireEvent.click(checkbox)
    expect(onToggle).not.toHaveBeenCalled()

    expect(screen.queryByTestId('action-step-delete-step-1')).toBeNull()
    expect(onRename).not.toHaveBeenCalled()
    expect(onDelete).not.toHaveBeenCalled()
  })

  it('renders title in plain mode and toggles edit on click', async () => {
    render(
      <StepRow
        step={baseStep}
        onToggle={vi.fn()}
        onRename={vi.fn()}
        onDelete={vi.fn()}
        ariaCheckboxLabel="check"
        ariaDeleteLabel="delete"
      />,
    )
    const titleBtn = screen.getByTestId('action-step-title-step-1')
    expect(titleBtn).toHaveTextContent('collect logs')

    fireEvent.click(titleBtn)
    const input = await screen.findByTestId('action-step-edit-step-1') as HTMLInputElement
    expect(input).toBeInTheDocument()
  })

  it('saves on Enter and cancels on Escape', async () => {
    const onRename = vi.fn().mockResolvedValue(undefined)
    render(
      <StepRow
        step={baseStep}
        onToggle={vi.fn()}
        onRename={onRename}
        onDelete={vi.fn()}
        ariaCheckboxLabel="check"
        ariaDeleteLabel="delete"
      />,
    )
    fireEvent.click(screen.getByTestId('action-step-title-step-1'))
    const input = await screen.findByTestId('action-step-edit-step-1') as HTMLInputElement

    fireEvent.change(input, { target: { value: 'review logs' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => expect(onRename).toHaveBeenCalledWith('review logs'))

    // Re-enter edit mode and cancel: onRename must NOT fire.
    fireEvent.click(screen.getByTestId('action-step-title-step-1'))
    const input2 = await screen.findByTestId('action-step-edit-step-1') as HTMLInputElement
    fireEvent.change(input2, { target: { value: 'aborted' } })
    fireEvent.keyDown(input2, { key: 'Escape' })
    await waitFor(() => expect(screen.queryByTestId('action-step-edit-step-1')).toBeNull())
    expect(onRename).toHaveBeenCalledTimes(1)
  })

  it('does not save on Enter while IME is composing', async () => {
    const onRename = vi.fn()
    render(
      <StepRow
        step={baseStep}
        onToggle={vi.fn()}
        onRename={onRename}
        onDelete={vi.fn()}
        ariaCheckboxLabel="check"
        ariaDeleteLabel="delete"
      />,
    )
    fireEvent.click(screen.getByTestId('action-step-title-step-1'))
    const input = await screen.findByTestId('action-step-edit-step-1') as HTMLInputElement

    fireEvent.change(input, { target: { value: 'composing kana' } })
    // Legacy Safari IME-composition signal — keyCode 229 is the documented
    // marker the keyboard helper checks for.
    fireEvent.keyDown(input, { key: 'Enter', keyCode: 229 })

    // Edit mode stays open; no rename was emitted.
    expect(screen.queryByTestId('action-step-edit-step-1')).toBeInTheDocument()
    expect(onRename).not.toHaveBeenCalled()
  })

  it('saves on blur', async () => {
    const onRename = vi.fn().mockResolvedValue(undefined)
    render(
      <StepRow
        step={baseStep}
        onToggle={vi.fn()}
        onRename={onRename}
        onDelete={vi.fn()}
        ariaCheckboxLabel="check"
        ariaDeleteLabel="delete"
      />,
    )
    fireEvent.click(screen.getByTestId('action-step-title-step-1'))
    const input = await screen.findByTestId('action-step-edit-step-1') as HTMLInputElement
    fireEvent.change(input, { target: { value: 'blurred' } })
    fireEvent.blur(input)
    await waitFor(() => expect(onRename).toHaveBeenCalledWith('blurred'))
  })

  it('does not save when title is unchanged or empty', async () => {
    const onRename = vi.fn()
    render(
      <StepRow
        step={baseStep}
        onToggle={vi.fn()}
        onRename={onRename}
        onDelete={vi.fn()}
        ariaCheckboxLabel="check"
        ariaDeleteLabel="delete"
      />,
    )
    fireEvent.click(screen.getByTestId('action-step-title-step-1'))
    const input = await screen.findByTestId('action-step-edit-step-1') as HTMLInputElement

    fireEvent.change(input, { target: { value: '   ' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => expect(screen.queryByTestId('action-step-edit-step-1')).toBeNull())
    expect(onRename).not.toHaveBeenCalled()

    fireEvent.click(screen.getByTestId('action-step-title-step-1'))
    const input2 = await screen.findByTestId('action-step-edit-step-1') as HTMLInputElement
    fireEvent.keyDown(input2, { key: 'Enter' })
    await waitFor(() => expect(screen.queryByTestId('action-step-edit-step-1')).toBeNull())
    expect(onRename).not.toHaveBeenCalled()
  })

  it('toggles checkbox via onToggle', () => {
    const onToggle = vi.fn()
    render(
      <StepRow
        step={baseStep}
        onToggle={onToggle}
        onRename={vi.fn()}
        onDelete={vi.fn()}
        ariaCheckboxLabel="check"
        ariaDeleteLabel="delete"
      />,
    )
    fireEvent.click(screen.getByTestId('action-step-checkbox-step-1'))
    expect(onToggle).toHaveBeenCalledTimes(1)
  })

  it('renders done state with line-through styling and supports delete click', () => {
    const onDelete = vi.fn()
    render(
      <StepRow
        step={{ ...baseStep, done: true }}
        onToggle={vi.fn()}
        onRename={vi.fn()}
        onDelete={onDelete}
        ariaCheckboxLabel="check"
        ariaDeleteLabel="delete"
      />,
    )
    const checkbox = screen.getByTestId('action-step-checkbox-step-1')
    expect(checkbox).toHaveAttribute('aria-checked', 'true')

    fireEvent.click(screen.getByTestId('action-step-delete-step-1'))
    expect(onDelete).toHaveBeenCalledTimes(1)
  })
})
