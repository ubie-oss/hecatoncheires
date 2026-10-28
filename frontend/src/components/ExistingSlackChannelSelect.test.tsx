import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { useState } from 'react'
import { I18nProvider } from '../i18n'
import { GET_EXISTING_SLACK_CHANNEL_CANDIDATES } from '../graphql/case'
import ExistingSlackChannelSelect, { type SlackChannelCandidate } from './ExistingSlackChannelSelect'

const WORKSPACE_ID = 'risk'

const CANDIDATES: SlackChannelCandidate[] = [
  { id: 'C1', name: 'Incident-DB', isPrivate: false },
  { id: 'G1', name: 'incident-payroll', isPrivate: true },
  { id: 'C2', name: 'general', isPrivate: false },
]

function candidatesMock(counter: { calls: number }, candidates: SlackChannelCandidate[]): MockedResponse {
  return {
    request: { query: GET_EXISTING_SLACK_CHANNEL_CANDIDATES, variables: { workspaceId: WORKSPACE_ID } },
    result: () => {
      counter.calls++
      return { data: { existingSlackChannelCandidates: candidates } }
    },
    maxUsageCount: 10,
  }
}

// Holds the selection the way CaseForm does, so choosing an option updates the
// component's value.
function Harness({ onChange }: { onChange?: (c: SlackChannelCandidate | null) => void }) {
  const [value, setValue] = useState<SlackChannelCandidate | null>(null)
  return (
    <ExistingSlackChannelSelect
      workspaceId={WORKSPACE_ID}
      value={value}
      onChange={(c) => { setValue(c); onChange?.(c) }}
      caseLabelLower="case"
      refetchKey={0}
    />
  )
}

function renderSelect(mocks: MockedResponse[], onChange?: (c: SlackChannelCandidate | null) => void) {
  return render(
    <MockedProvider mocks={mocks} addTypename={false}>
      <I18nProvider defaultLang="en">
        <Harness onChange={onChange} />
      </I18nProvider>
    </MockedProvider>,
  )
}

async function openMenu() {
  const combobox = await screen.findByRole('combobox', { name: 'Slack channel' })
  fireEvent.keyDown(combobox, { key: 'ArrowDown' })
  return combobox
}

afterEach(() => cleanup())

describe('ExistingSlackChannelSelect', () => {
  it('explains how to make a channel available when there are no candidates and offers a retry', async () => {
    const counter = { calls: 0 }
    renderSelect([candidatesMock(counter, [])])
    await screen.findByRole('combobox', { name: 'Slack channel' })
    expect(screen.getByTestId('existing-slack-channel-empty')).toHaveTextContent(
      'No channels are available. Join the channel you want to use, then press Retry. For a private channel, also invite the bot.',
    )
    expect(counter.calls).toBe(1)
    fireEvent.click(screen.getByTestId('existing-slack-channel-retry'))
    await waitFor(() => expect(counter.calls).toBe(2))
  })

  it('tells the user when the search text matches no channel', async () => {
    renderSelect([candidatesMock({ calls: 0 }, CANDIDATES)])
    const combobox = await openMenu()
    fireEvent.change(combobox, { target: { value: 'zzz' } })
    expect(await screen.findByText('No channels match your search.')).toBeInTheDocument()
    expect(screen.queryByTestId('existing-slack-channel-empty')).not.toBeInTheDocument()
  })

  it('shows a load error with a retry button that fetches again', async () => {
    const counter = { calls: 0 }
    renderSelect([
      {
        request: { query: GET_EXISTING_SLACK_CHANNEL_CANDIDATES, variables: { workspaceId: WORKSPACE_ID } },
        error: new Error('slack failed'),
      },
      candidatesMock(counter, CANDIDATES),
    ])
    expect(await screen.findByTestId('existing-slack-channel-load-error')).toHaveTextContent(
      'Could not load the channel list.',
    )
    fireEvent.click(screen.getByTestId('existing-slack-channel-retry'))
    await screen.findByRole('combobox', { name: 'Slack channel' })
    expect(counter.calls).toBe(1)
  })

  it('filters candidates by name ignoring case and marks private channels', async () => {
    renderSelect([candidatesMock({ calls: 0 }, CANDIDATES)])
    const combobox = await openMenu()
    fireEvent.change(combobox, { target: { value: 'INCIDENT' } })

    const options = await screen.findAllByRole('option')
    expect(options.map((o) => o.textContent)).toEqual(['#Incident-DB', 'incident-payroll'])
    const privateOption = screen.getByRole('option', { name: 'incident-payroll' })
    expect(within(privateOption).getByTestId('existing-slack-channel-private-icon')).toBeInTheDocument()
    const publicOption = screen.getByRole('option', { name: 'Incident-DB' })
    expect(within(publicOption).queryByTestId('existing-slack-channel-private-icon')).not.toBeInTheDocument()
  })

  it('does not match the channel ID', async () => {
    renderSelect([candidatesMock({ calls: 0 }, CANDIDATES)])
    const combobox = await openMenu()
    fireEvent.change(combobox, { target: { value: 'G1' } })
    expect(await screen.findByText('No channels match your search.')).toBeInTheDocument()
    expect(screen.queryAllByRole('option')).toHaveLength(0)
  })

  it.each([
    ['Incident-DB', 'This is a public channel, so this case will not be private.'],
    ['incident-payroll', 'This is a private channel, so this case will be private and visible only to channel members.'],
  ])('shows the visibility that follows from choosing %s', async (name, hint) => {
    const onChange = vi.fn()
    renderSelect([candidatesMock({ calls: 0 }, CANDIDATES)], onChange)
    await openMenu()
    expect(screen.queryByTestId('existing-slack-channel-visibility')).not.toBeInTheDocument()

    fireEvent.click(await screen.findByRole('option', { name }))
    expect(onChange).toHaveBeenCalledWith(CANDIDATES.find((c) => c.name === name))
    expect(screen.getByTestId('existing-slack-channel-visibility')).toHaveTextContent(hint)
  })
})
