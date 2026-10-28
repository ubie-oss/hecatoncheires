import { afterEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { GraphQLError } from 'graphql'
import { MemoryRouter } from 'react-router'
import { I18nProvider } from '../i18n'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'
import { CREATE_CASE, GET_EXISTING_SLACK_CHANNEL_CANDIDATES } from '../graphql/case'
import { GET_FREQUENT_ASSIGNEE_IDS, GET_SLACK_USERS } from '../graphql/slackUsers'
import { GET_CASE_STATUS_CONFIG } from '../graphql/caseStatus'
import CaseForm from './CaseForm'

const WORKSPACE_ID = 'risk'

// currentWorkspace is mutable so a test can simulate the initial
// workspace-context load, where it is undefined.
let mockCurrentWorkspace: { id: string; name: string } | undefined = {
  id: WORKSPACE_ID,
  name: 'Risk',
}

vi.mock('../contexts/workspace-context', () => ({
  useWorkspace: () => ({
    currentWorkspace: mockCurrentWorkspace,
    workspaces: mockCurrentWorkspace ? [mockCurrentWorkspace] : [],
    isLoading: false,
    setCurrentWorkspace: vi.fn(),
    switchWorkspace: vi.fn(),
  }),
}))

function fieldConfigMock(allowExistingSlackChannel = false): MockedResponse {
  return {
    request: { query: GET_FIELD_CONFIGURATION, variables: { workspaceId: WORKSPACE_ID } },
    result: {
      data: {
        fieldConfiguration: {
          fields: [],
          labels: { case: 'Case' },
          actionConfig: { initial: 'BACKLOG', closed: ['COMPLETED'], statuses: [] },
          allowExistingSlackChannel,
        },
      },
    },
  }
}

function slackUsersMock(): MockedResponse {
  return {
    request: { query: GET_SLACK_USERS },
    result: { data: { slackUsers: [] } },
  }
}

// The assignee picker orders its candidates by this ranking; the form itself has
// no user to rank here, so an empty list is enough to satisfy the query.
function frequentAssigneesMock(): MockedResponse {
  return {
    request: { query: GET_FREQUENT_ASSIGNEE_IDS, variables: { workspaceId: WORKSPACE_ID } },
    result: { data: { frequentAssigneeIDs: [] } },
  }
}

// channelStatusMock resolves caseStatusConfig to null — the shape a channel-mode
// workspace returns (no configurable Case status set / Kanban).
function channelStatusMock(): MockedResponse {
  return {
    request: { query: GET_CASE_STATUS_CONFIG, variables: { workspaceId: WORKSPACE_ID } },
    result: { data: { caseStatusConfig: null } },
  }
}

// threadStatusMock resolves caseStatusConfig to a populated status set — only a
// thread-mode workspace exposes one.
function threadStatusMock(): MockedResponse {
  return {
    request: { query: GET_CASE_STATUS_CONFIG, variables: { workspaceId: WORKSPACE_ID } },
    result: {
      data: {
        caseStatusConfig: {
          initial: 'triage',
          closed: ['done'],
          statuses: [
            { id: 'triage', name: 'Triage', description: '', color: '#888', emoji: null },
            { id: 'done', name: 'Done', description: '', color: '#0a0', emoji: null },
          ],
        },
      },
    },
  }
}

// erroredStatusMock makes the status query fail — the mode is then UNKNOWN.
function erroredStatusMock(): MockedResponse {
  return {
    request: { query: GET_CASE_STATUS_CONFIG, variables: { workspaceId: WORKSPACE_ID } },
    error: new Error('status config query failed'),
  }
}

// pendingStatusMock never resolves within the test window, keeping the status
// query in its loading state.
function pendingStatusMock(): MockedResponse {
  return {
    request: { query: GET_CASE_STATUS_CONFIG, variables: { workspaceId: WORKSPACE_ID } },
    result: { data: { caseStatusConfig: null } },
    delay: 100_000,
  }
}

function renderForm(statusMock: MockedResponse) {
  return render(
    <MemoryRouter initialEntries={[`/ws/${WORKSPACE_ID}/cases`]}>
      <MockedProvider
        mocks={[fieldConfigMock(), slackUsersMock(), frequentAssigneesMock(), statusMock]}
        addTypename={false}
      >
        <I18nProvider defaultLang="en">
          <CaseForm caseItem={null} onClose={vi.fn()} />
        </I18nProvider>
      </MockedProvider>
    </MemoryRouter>,
  )
}

const CANDIDATES = [
  { id: 'C1', name: 'incident-db', isPrivate: false },
  { id: 'G1', name: 'incident-payroll', isPrivate: true },
]

// candidatesMock answers the candidate query; counter records each request.
function candidatesMock(counter: { calls: number }, candidates = CANDIDATES): MockedResponse {
  return {
    request: { query: GET_EXISTING_SLACK_CHANNEL_CANDIDATES, variables: { workspaceId: WORKSPACE_ID } },
    result: () => {
      counter.calls++
      return { data: { existingSlackChannelCandidates: candidates } }
    },
    maxUsageCount: 10,
  }
}

const createdCase = {
  id: 1,
  workspaceId: WORKSPACE_ID,
  title: 'T',
  description: '',
  status: 'OPEN',
  archivedAt: null,
  isPrivate: false,
  isTest: false,
  accessDenied: false,
  reporterID: null,
  reporter: null,
  assigneeIDs: [],
  assignees: [],
  slackChannelID: 'C1',
  createdAt: '2026-10-28T00:00:00Z',
  updatedAt: '2026-10-28T00:00:00Z',
  fields: [],
}

// createCaseMock records the createCase input it receives. errorCode, when
// set, answers with a GraphQL error carrying that extensions.code.
function createCaseMock(captured: { inputs: any[] }, errorCode?: string): MockedResponse {
  return {
    request: { query: CREATE_CASE },
    variableMatcher: (vars: any) => {
      captured.inputs.push(vars.input)
      return true
    },
    result: errorCode
      ? { errors: [new GraphQLError('rejected', { extensions: { code: errorCode } })] }
      : { data: { createCase: createdCase } },
    maxUsageCount: 10,
  } as MockedResponse
}

function renderChannelForm(extraMocks: MockedResponse[], allowExisting = true) {
  return render(
    <MemoryRouter initialEntries={[`/ws/${WORKSPACE_ID}/cases`]}>
      <MockedProvider
        mocks={[fieldConfigMock(allowExisting), slackUsersMock(), frequentAssigneesMock(), channelStatusMock(), ...extraMocks]}
        addTypename={false}
      >
        <I18nProvider defaultLang="en">
          <CaseForm caseItem={null} onClose={vi.fn()} />
        </I18nProvider>
      </MockedProvider>
    </MemoryRouter>,
  )
}

async function chooseExistingChannel(name: string) {
  fireEvent.click(await screen.findByTestId('case-channel-source-existing'))
  const combobox = await screen.findByRole('combobox', { name: 'Slack channel' })
  fireEvent.keyDown(combobox, { key: 'ArrowDown' })
  fireEvent.click(await screen.findByRole('option', { name }))
}

// flush lets MockedProvider deliver its (macrotask-scheduled) query results and
// React apply the resulting state updates, all inside act().
async function flush() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0))
  })
}

afterEach(() => {
  cleanup()
  mockCurrentWorkspace = { id: WORKSPACE_ID, name: 'Risk' }
})

describe('CaseForm private-case toggle visibility', () => {
  it('shows the Private toggle only once channel mode is confirmed', async () => {
    renderForm(channelStatusMock())
    // The form renders immediately; the toggle appears after the status query
    // resolves and confirms channel mode.
    expect(await screen.findByTestId('private-case-checkbox')).toBeInTheDocument()
  })

  it('hides the Private toggle in thread mode', async () => {
    renderForm(threadStatusMock())
    await screen.findByTestId('case-title-input')
    await flush()
    expect(screen.queryByTestId('private-case-checkbox')).not.toBeInTheDocument()
  })

  it('hides the Private toggle while the workspace mode is still loading', async () => {
    renderForm(pendingStatusMock())
    await screen.findByTestId('case-title-input')
    await flush()
    // The status query is still in flight, so the mode is not yet known.
    expect(screen.queryByTestId('private-case-checkbox')).not.toBeInTheDocument()
  })

  it('hides the Private toggle when the mode query errors (mode unknown)', async () => {
    renderForm(erroredStatusMock())
    await screen.findByTestId('case-title-input')
    await flush()
    expect(screen.queryByTestId('private-case-checkbox')).not.toBeInTheDocument()
  })

  it('hides the Private toggle during initial load (workspace undefined)', async () => {
    // With no workspace yet, useCaseStatuses SKIPS its query — a skipped Apollo
    // query reports loading:false / error:undefined, so the toggle must be kept
    // hidden by the workspace-id gate rather than flashing on.
    mockCurrentWorkspace = undefined
    renderForm(channelStatusMock())
    await screen.findByTestId('case-title-input')
    await flush()
    expect(screen.queryByTestId('private-case-checkbox')).not.toBeInTheDocument()
  })
})

describe('CaseForm Slack channel source', () => {
  const fillTitle = async () => {
    fireEvent.change(await screen.findByTestId('case-title-input'), { target: { value: 'T' } })
  }

  it('is hidden when the workspace does not allow existing channels', async () => {
    renderChannelForm([], false)
    await screen.findByTestId('private-case-checkbox')
    expect(screen.queryByTestId('case-channel-source')).not.toBeInTheDocument()
  })

  it('is hidden in thread mode', async () => {
    render(
      <MemoryRouter initialEntries={[`/ws/${WORKSPACE_ID}/cases`]}>
        <MockedProvider
          mocks={[fieldConfigMock(true), slackUsersMock(), frequentAssigneesMock(), threadStatusMock()]}
          addTypename={false}
        >
          <I18nProvider defaultLang="en">
            <CaseForm caseItem={null} onClose={vi.fn()} />
          </I18nProvider>
        </MockedProvider>
      </MemoryRouter>,
    )
    await screen.findByTestId('case-title-input')
    await flush()
    expect(screen.queryByTestId('case-channel-source')).not.toBeInTheDocument()
  })

  it.each([
    ['an open case', 'OPEN' as const],
    ['a draft', 'DRAFT' as const],
  ])('is hidden when editing %s', async (_, status) => {
    render(
      <MemoryRouter initialEntries={[`/ws/${WORKSPACE_ID}/cases`]}>
        <MockedProvider
          mocks={[fieldConfigMock(true), slackUsersMock(), frequentAssigneesMock(), channelStatusMock()]}
          addTypename={false}
        >
          <I18nProvider defaultLang="en">
            <CaseForm
              caseItem={{ id: 1, title: 'T', description: '', isPrivate: false, assigneeIDs: [], fields: [], status }}
              onClose={vi.fn()}
            />
          </I18nProvider>
        </MockedProvider>
      </MemoryRouter>,
    )
    await screen.findByTestId('case-title-input')
    await flush()
    expect(screen.queryByTestId('case-channel-source')).not.toBeInTheDocument()
  })

  it('defaults to a new channel and does not fetch candidates', async () => {
    const counter = { calls: 0 }
    renderChannelForm([candidatesMock(counter)])
    expect(await screen.findByTestId('case-channel-source-new')).toBeChecked()
    await flush()
    expect(counter.calls).toBe(0)
    expect(screen.getByTestId('private-case-checkbox')).toBeInTheDocument()
  })

  it('shows a loading text while the candidates are being fetched', async () => {
    // The response never arrives within the test, so the loading state stays.
    renderChannelForm([{ ...candidatesMock({ calls: 0 }), delay: 100_000 }])
    fireEvent.click(await screen.findByTestId('case-channel-source-existing'))
    expect(await screen.findByText('Loading channels...')).toBeInTheDocument()
    expect(screen.queryByRole('combobox', { name: 'Slack channel' })).not.toBeInTheDocument()
  })

  it('fetches the candidates once when an existing channel is chosen', async () => {
    const counter = { calls: 0 }
    renderChannelForm([candidatesMock(counter)])
    fireEvent.click(await screen.findByTestId('case-channel-source-existing'))
    await screen.findByRole('combobox', { name: 'Slack channel' })
    await flush()
    expect(counter.calls).toBe(1)
  })

  it('sends existingSlackChannelID without isPrivate and hides the private toggle', async () => {
    const captured = { inputs: [] as any[] }
    renderChannelForm([candidatesMock({ calls: 0 }), createCaseMock(captured)])
    await fillTitle()
    await chooseExistingChannel('incident-db')
    expect(screen.queryByTestId('private-case-checkbox')).not.toBeInTheDocument()
    expect(screen.getByTestId('existing-slack-channel-visibility')).toHaveTextContent(
      'This is a public channel, so this case will not be private.',
    )

    fireEvent.click(screen.getByTestId('case-submit-button'))
    await waitFor(() => expect(captured.inputs).toHaveLength(1))
    expect(captured.inputs[0].existingSlackChannelID).toBe('C1')
    expect('isPrivate' in captured.inputs[0]).toBe(false)
  })

  it('never sends isPrivate for an existing channel and restores the toggle when switching back', async () => {
    const captured = { inputs: [] as any[] }
    renderChannelForm([candidatesMock({ calls: 0 }), createCaseMock(captured)])
    await fillTitle()
    fireEvent.click(await screen.findByTestId('private-case-checkbox'))

    fireEvent.click(await screen.findByTestId('case-channel-source-existing'))
    const combobox = await screen.findByRole('combobox', { name: 'Slack channel' })
    // No channel chosen yet: no visibility text.
    expect(screen.queryByTestId('existing-slack-channel-visibility')).not.toBeInTheDocument()

    fireEvent.keyDown(combobox, { key: 'ArrowDown' })
    fireEvent.click(await screen.findByRole('option', { name: 'incident-payroll' }))
    expect(screen.getByTestId('existing-slack-channel-visibility')).toHaveTextContent(
      'This is a private channel, so this case will be private and visible only to channel members.',
    )

    fireEvent.click(screen.getByTestId('case-submit-button'))
    await waitFor(() => expect(captured.inputs).toHaveLength(1))
    expect(captured.inputs[0].existingSlackChannelID).toBe('G1')
    expect('isPrivate' in captured.inputs[0]).toBe(false)

    fireEvent.click(screen.getByTestId('case-channel-source-new'))
    expect(screen.getByTestId('private-case-checkbox')).toBeChecked()
  })

  it('disables save-as-draft while an existing channel is chosen', async () => {
    const captured = { inputs: [] as any[] }
    renderChannelForm([candidatesMock({ calls: 0 }), createCaseMock(captured)])
    await fillTitle()
    fireEvent.click(await screen.findByTestId('case-channel-source-existing'))
    expect(screen.getByTestId('case-save-as-draft-button')).toBeDisabled()
    expect(screen.getByTestId('case-draft-unavailable-hint')).toHaveTextContent(
      'You cannot save as a draft when using an existing channel.',
    )

    fireEvent.click(screen.getByTestId('case-channel-source-new'))
    expect(screen.getByTestId('case-save-as-draft-button')).not.toBeDisabled()
    fireEvent.click(screen.getByTestId('case-submit-button'))
    await waitFor(() => expect(captured.inputs).toHaveLength(1))
    expect('existingSlackChannelID' in captured.inputs[0]).toBe(false)
    expect(captured.inputs[0].isPrivate).toBe(false)
  })

  it('requires a channel before creating', async () => {
    const captured = { inputs: [] as any[] }
    renderChannelForm([candidatesMock({ calls: 0 }), createCaseMock(captured)])
    await fillTitle()
    fireEvent.click(await screen.findByTestId('case-channel-source-existing'))
    await screen.findByRole('combobox', { name: 'Slack channel' })
    fireEvent.click(screen.getByTestId('case-submit-button'))
    expect(await screen.findByTestId('existing-slack-channel-error')).toHaveTextContent('Select a channel.')
    await flush()
    expect(captured.inputs).toHaveLength(0)
  })

  it.each([
    ['SLACK_CHANNEL_ALREADY_LINKED', 'This channel is already linked to another case. Choose a different channel.'],
    ['SLACK_CHANNEL_NOT_LINKABLE', 'This channel cannot be used. Choose a channel that you have joined and that is not archived. For a private channel, also invite the bot.'],
    ['SLACK_CHANNEL_JOIN_FAILED', 'The bot could not join this channel. Invite the bot to the channel and try again.'],
  ])('shows %s under the picker and fetches the candidates again', async (code, message) => {
    const counter = { calls: 0 }
    const captured = { inputs: [] as any[] }
    renderChannelForm([candidatesMock(counter), createCaseMock(captured, code)])
    await fillTitle()
    await chooseExistingChannel('incident-db')
    expect(counter.calls).toBe(1)

    fireEvent.click(screen.getByTestId('case-submit-button'))
    expect(await screen.findByTestId('existing-slack-channel-error')).toHaveTextContent(message)
    await waitFor(() => expect(counter.calls).toBe(2))
  })
})
