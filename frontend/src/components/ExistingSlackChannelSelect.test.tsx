import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { useState } from 'react'
import { I18nProvider } from '../i18n'
import { GET_EXISTING_SLACK_CHANNEL_CANDIDATES } from '../graphql/case'
import ExistingSlackChannelSelect, { type SlackChannelCandidate } from './ExistingSlackChannelSelect'

const WORKSPACE_ID = 'risk'
const FETCHED_AT = '2026-10-28T03:15:00Z'

const INCIDENT_CHANNELS: SlackChannelCandidate[] = [
  { id: 'C1', name: 'Incident-DB', isPrivate: false },
  { id: 'G1', name: 'incident-payroll', isPrivate: true },
]
const ALL_CHANNELS: SlackChannelCandidate[] = [...INCIDENT_CHANNELS, { id: 'C2', name: 'general', isPrivate: false }]

interface SearchMock {
  query?: string
  refresh?: boolean
  channels?: SlackChannelCandidate[]
  hasMore?: boolean
  counter?: { calls: number }
  error?: Error
}

// searchMock answers one search the way the server does: the server has
// already matched the channels, so the mock returns exactly what to show.
function searchMock({ query = '', refresh, channels = [], hasMore = false, counter, error }: SearchMock): MockedResponse {
  const variables: Record<string, unknown> = { workspaceId: WORKSPACE_ID, query }
  if (refresh !== undefined) variables.refresh = refresh
  if (error) {
    return { request: { query: GET_EXISTING_SLACK_CHANNEL_CANDIDATES, variables }, error }
  }
  return {
    request: { query: GET_EXISTING_SLACK_CHANNEL_CANDIDATES, variables },
    result: () => {
      if (counter) counter.calls++
      return { data: { existingSlackChannelCandidates: { channels, hasMore, fetchedAt: FETCHED_AT } } }
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
  it('sends the typed text to the server and shows the channels it matched, marking private ones', async () => {
    const counter = { calls: 0 }
    renderSelect([
      searchMock({ channels: ALL_CHANNELS }),
      searchMock({ query: 'INCIDENT', channels: INCIDENT_CHANNELS, counter }),
    ])
    const combobox = await openMenu()
    fireEvent.change(combobox, { target: { value: '  INCIDENT ' } })

    await waitFor(() => expect(counter.calls).toBe(1))
    await waitFor(() =>
      expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual(['#Incident-DB', 'incident-payroll']),
    )
    const privateOption = screen.getByRole('option', { name: 'incident-payroll' })
    expect(within(privateOption).getByTestId('existing-slack-channel-private-icon')).toBeInTheDocument()
    const publicOption = screen.getByRole('option', { name: 'Incident-DB' })
    expect(within(publicOption).queryByTestId('existing-slack-channel-private-icon')).not.toBeInTheDocument()
  })

  it('tells the user when the search text matches no channel', async () => {
    renderSelect([searchMock({ channels: ALL_CHANNELS }), searchMock({ query: 'zzz' })])
    const combobox = await openMenu()
    fireEvent.change(combobox, { target: { value: 'zzz' } })
    expect(
      await screen.findByText('No channels match. A private channel appears only after the bot is invited.'),
    ).toBeInTheDocument()
    expect(screen.queryByTestId('existing-slack-channel-empty')).not.toBeInTheDocument()
  })

  it('asks the user to narrow the search under the options when more channels match than are shown', async () => {
    renderSelect([searchMock({ channels: ALL_CHANNELS, hasMore: true })])
    await openMenu()
    expect(await screen.findByTestId('existing-slack-channel-has-more')).toHaveTextContent(
      'More channels match. Type more to narrow the list.',
    )
  })

  it('shows no narrowing note when every match is listed', async () => {
    renderSelect([searchMock({ channels: ALL_CHANNELS })])
    await openMenu()
    await screen.findByRole('option', { name: 'general' })
    expect(screen.queryByTestId('existing-slack-channel-has-more')).not.toBeInTheDocument()
  })

  it('says on one line when the channel list was fetched and reloads it from Slack before searching again', async () => {
    const refreshCounter = { calls: 0 }
    const searchCounter = { calls: 0 }
    renderSelect([
      searchMock({ channels: ALL_CHANNELS, counter: searchCounter }),
      searchMock({ refresh: true, channels: ALL_CHANNELS, counter: refreshCounter }),
    ])
    const fetchedAt = await screen.findByTestId('existing-slack-channel-fetched-at')
    expect(fetchedAt).toHaveTextContent(/^Your channels as of .+$/)
    expect(fetchedAt.parentElement).toHaveTextContent(/^Your channels as of .+ · Reload$/)
    expect(searchCounter.calls).toBe(1)

    fireEvent.click(screen.getByTestId('existing-slack-channel-reload'))
    await waitFor(() => expect(refreshCounter.calls).toBe(1))
    await waitFor(() => expect(searchCounter.calls).toBe(2))
  })

  it('shows an error when reloading from Slack fails and keeps the last result', async () => {
    renderSelect([
      searchMock({ channels: ALL_CHANNELS }),
      searchMock({ refresh: true, error: new Error('slack failed') }),
    ])
    fireEvent.click(await screen.findByTestId('existing-slack-channel-reload'))
    expect(await screen.findByTestId('existing-slack-channel-load-error')).toHaveTextContent(
      'Could not load the channel list.',
    )
    expect(screen.getByRole('combobox', { name: 'Slack channel' })).toBeInTheDocument()
  })

  it('explains how to make a channel available when there are no candidates and retries from Slack', async () => {
    const refreshCounter = { calls: 0 }
    renderSelect([searchMock({}), searchMock({ refresh: true, counter: refreshCounter })])
    expect(await screen.findByTestId('existing-slack-channel-empty')).toHaveTextContent(
      'No channels are available. Join the channel you want to use, then press Retry. For a private channel, also invite the bot.',
    )
    fireEvent.click(screen.getByTestId('existing-slack-channel-retry'))
    await waitFor(() => expect(refreshCounter.calls).toBe(1))
  })

  it('shows a load error with a retry button that fetches again', async () => {
    const counter = { calls: 0 }
    renderSelect([
      searchMock({ error: new Error('slack failed') }),
      searchMock({ channels: ALL_CHANNELS, counter }),
    ])
    expect(await screen.findByTestId('existing-slack-channel-load-error')).toHaveTextContent(
      'Could not load the channel list.',
    )
    fireEvent.click(screen.getByTestId('existing-slack-channel-retry'))
    await screen.findByRole('combobox', { name: 'Slack channel' })
    expect(counter.calls).toBe(1)
  })

  it.each([
    ['Incident-DB', 'This is a public channel, so this case will not be private.'],
    ['incident-payroll', 'This is a private channel, so this case will be private and visible only to channel members.'],
  ])('shows the visibility that follows from choosing %s', async (name, hint) => {
    const onChange = vi.fn()
    renderSelect([searchMock({ channels: ALL_CHANNELS })], onChange)
    await openMenu()
    expect(screen.queryByTestId('existing-slack-channel-visibility')).not.toBeInTheDocument()

    fireEvent.click(await screen.findByRole('option', { name }))
    expect(onChange).toHaveBeenCalledWith(ALL_CHANNELS.find((c) => c.name === name))
    expect(screen.getByTestId('existing-slack-channel-visibility')).toHaveTextContent(hint)
  })
})
