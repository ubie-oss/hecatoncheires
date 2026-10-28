import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, within, cleanup } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { MemoryRouter, Routes, Route, useLocation } from 'react-router'
import { I18nProvider } from '../i18n'
import { BULK_ARCHIVE_ACTIONS, GET_ACTIONS_BY_CASE, GET_OPEN_CASE_ACTIONS } from '../graphql/action'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'
import { GET_CASES } from '../graphql/case'
import ActionList from './ActionList'

// Pin the workspace context so the page renders without a real fetch.
vi.mock('../contexts/workspace-context', () => ({
  useWorkspace: () => ({
    currentWorkspace: { id: 'risk', name: 'Risk' },
    workspaces: [{ id: 'risk', name: 'Risk' }],
    isLoading: false,
    setCurrentWorkspace: vi.fn(),
    switchWorkspace: vi.fn(),
  }),
}))

// ActionModal is not under test here and pulls in heavier queries; stub it.
vi.mock('./ActionModal', () => ({
  default: ({ actionId }: { actionId: number }) => (
    <div data-testid="action-modal">action-modal:{actionId}</div>
  ),
}))

// ActionForm is not under test here.
vi.mock('./ActionForm', () => ({
  default: () => <div data-testid="action-form" />,
}))

const fieldConfigMock = {
  request: {
    query: GET_FIELD_CONFIGURATION,
    variables: { workspaceId: 'risk' },
  },
  result: {
    data: {
      fieldConfiguration: {
        __typename: 'FieldConfiguration',
        fields: [],
        labels: { __typename: 'FieldLabels', case: 'Case' },
        actionConfig: {
          __typename: 'ActionConfig',
          initial: 'BACKLOG',
          closed: ['COMPLETED'],
          statuses: [
            { __typename: 'ActionStatus', id: 'BACKLOG', name: 'Backlog', description: null, color: 'idle', emoji: null },
            { __typename: 'ActionStatus', id: 'TODO', name: 'To Do', description: null, color: 'idle', emoji: null },
            { __typename: 'ActionStatus', id: 'IN_PROGRESS', name: 'In Progress', description: null, color: 'active', emoji: null },
            { __typename: 'ActionStatus', id: 'BLOCKED', name: 'Blocked', description: null, color: 'blocked', emoji: null },
            { __typename: 'ActionStatus', id: 'COMPLETED', name: 'Completed', description: null, color: 'success', emoji: null },
          ],
        },
        allowExistingSlackChannel: false,
      },
    },
  },
}

const actionRow = (
  id: number,
  caseID: number,
  caseTitle: string,
  title: string,
  status = 'IN_PROGRESS',
) => ({
  __typename: 'Action',
  id,
  caseID,
  case: { __typename: 'Case', id: caseID, title: caseTitle, slackChannelID: null, slackChannelURL: null },
  title,
  description: '',
  assigneeID: null,
  assignee: null,
  slackMessageTS: null,
  status,
  dueDate: null,
  archived: false,
  archivedAt: null,
  createdAt: '2026-05-01T00:00:00Z',
  updatedAt: '2026-05-01T00:00:00Z',
  stepProgress: { __typename: 'StepProgress', done: 0, total: 0 },
})

const allOpenActions = [
  actionRow(101, 3, 'GitHub incident', 'Build detection pipeline'),
  actionRow(102, 3, 'GitHub incident', 'Update WIF policy', 'TODO'),
  actionRow(103, 4, 'Other case', 'Unrelated action'),
]

const caseRow = (id: number, title: string) => ({
  __typename: 'Case',
  id,
  title,
  description: '',
  status: 'OPEN',
  isPrivate: false,
  accessDenied: false,
  reporterID: null,
  reporter: null,
  assigneeIDs: [],
  assignees: [],
  slackChannelID: null,
  slackChannelName: null,
  createdAt: '2026-04-01T00:00:00Z',
  updatedAt: '2026-04-01T00:00:00Z',
  fields: [],
})

const openCasesMock = {
  request: {
    query: GET_CASES,
    variables: { workspaceId: 'risk', status: 'OPEN' },
  },
  result: {
    data: {
      cases: [
        caseRow(3, 'GitHub incident'),
        caseRow(4, 'Other case'),
        caseRow(5, 'Email phishing'),
      ],
    },
  },
}

const openActionsMock = {
  request: {
    query: GET_OPEN_CASE_ACTIONS,
    variables: { workspaceId: 'risk' },
  },
  result: {
    data: { openCaseActions: allOpenActions },
  },
}

const actionsByCase3Mock = {
  request: {
    query: GET_ACTIONS_BY_CASE,
    variables: { workspaceId: 'risk', caseID: 3 },
  },
  result: {
    data: {
      actionsByCase: allOpenActions.filter((a) => a.caseID === 3),
    },
  },
}

let lastLocation = ''
function LocationProbe() {
  const loc = useLocation()
  lastLocation = `${loc.pathname}${loc.search}`
  return null
}

function renderAt(path: string, configMock = fieldConfigMock, extraMocks: MockedResponse[] = []) {
  const mocks = [configMock, openActionsMock, actionsByCase3Mock, openCasesMock, ...extraMocks]
  return render(
    <MemoryRouter initialEntries={[path]}>
      <MockedProvider mocks={mocks}>
        <I18nProvider defaultLang="en">
          <Routes>
            <Route path="/ws/:workspaceId/actions" element={<ActionList />} />
            <Route path="/ws/:workspaceId/actions/:actionId" element={<ActionList />} />
            <Route path="/ws/:workspaceId/actions/case/:caseId" element={<ActionList />} />
            <Route path="/ws/:workspaceId/actions/case/:caseId/:actionId" element={<ActionList />} />
          </Routes>
          <LocationProbe />
        </I18nProvider>
      </MockedProvider>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  lastLocation = ''
})

afterEach(() => {
  cleanup()
})

describe('ActionList case filter', () => {
  it('renders every action when no case filter is in the URL', async () => {
    renderAt('/ws/risk/actions')
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(3)
    })
    const trigger = screen.getByTestId('action-case-filter-trigger')
    expect(trigger).toHaveTextContent('All Case')
  })

  it('restricts the board to the matching case when /actions/case/:caseId is opened', async () => {
    renderAt('/ws/risk/actions/case/3')
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(2)
    })
    const chip = await screen.findByTestId('action-case-filter-chip')
    expect(chip).toHaveTextContent('#3')
    expect(chip).toHaveTextContent('GitHub incident')
  })

  it('navigates to the filtered URL when the case label on a card is clicked', async () => {
    renderAt('/ws/risk/actions')
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(3)
    })
    const card = screen.getAllByTestId('action-card')[0]
    const caseLink = within(card).getByTestId('action-card-case-link')
    fireEvent.click(caseLink)
    await waitFor(() => {
      expect(lastLocation).toBe('/ws/risk/actions/case/3')
    })
    expect(screen.queryByTestId('action-modal')).not.toBeInTheDocument()
  })

  it('navigates to a different case when the dropdown selection changes', async () => {
    renderAt('/ws/risk/actions/case/3')
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(2)
    })
    fireEvent.click(screen.getByTestId('action-case-filter-trigger'))
    fireEvent.click(await screen.findByTestId('action-case-filter-item-4'))
    await waitFor(() => {
      expect(lastLocation).toBe('/ws/risk/actions/case/4')
    })
  })

  it('returns to the unfiltered URL when the dropdown "All" item is chosen', async () => {
    renderAt('/ws/risk/actions/case/3')
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(2)
    })
    fireEvent.click(screen.getByTestId('action-case-filter-trigger'))
    fireEvent.click(await screen.findByTestId('action-case-filter-item-all'))
    await waitFor(() => {
      expect(lastLocation).toBe('/ws/risk/actions')
    })
  })

  it('ignores a non-numeric caseId and shows every action', async () => {
    renderAt('/ws/risk/actions/case/not-a-number')
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(3)
    })
    expect(screen.getByTestId('action-case-filter-trigger')).toHaveTextContent('All Case')
  })

  it('uses the workspace-configured case label on the dropdown', async () => {
    const customConfig = {
      ...fieldConfigMock,
      result: {
        data: {
          fieldConfiguration: {
            ...fieldConfigMock.result.data.fieldConfiguration,
            labels: { __typename: 'FieldLabels', case: 'Risk' },
          },
        },
      },
    }
    renderAt('/ws/risk/actions/case/3', customConfig)
    const trigger = await screen.findByTestId('action-case-filter-trigger')
    await waitFor(() => {
      expect(trigger).toHaveTextContent('Risk')
    })
    const chip = await screen.findByTestId('action-case-filter-chip')
    expect(chip).toHaveTextContent('#3')
    expect(chip).toHaveTextContent('GitHub incident')
  })

  it('combines text search with case filter (AND)', async () => {
    renderAt('/ws/risk/actions/case/3')
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(2)
    })
    const search = screen.getByTestId('action-search-input') as HTMLInputElement
    fireEvent.change(search, { target: { value: 'WIF' } })
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(1)
    })
    expect(screen.getByText('Update WIF policy')).toBeInTheDocument()
  })

  it('filters the dropdown items by the in-popup search input', async () => {
    renderAt('/ws/risk/actions')
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(3)
    })
    fireEvent.click(screen.getByTestId('action-case-filter-trigger'))
    const input = await screen.findByTestId('action-case-filter-search') as HTMLInputElement
    fireEvent.change(input, { target: { value: 'email' } })
    expect(screen.queryByTestId('action-case-filter-item-3')).toBeNull()
    expect(screen.queryByTestId('action-case-filter-item-4')).toBeNull()
    expect(screen.getByTestId('action-case-filter-item-5')).toBeInTheDocument()
  })
})

describe('ActionList completed-column bulk archive', () => {
  // Two completed actions plus the standard open actions so the COMPLETED
  // column has cards while other columns also remain populated.
  const completedA = actionRow(201, 3, 'GitHub incident', 'Finished alpha', 'COMPLETED')
  const completedB = actionRow(202, 3, 'GitHub incident', 'Finished beta', 'COMPLETED')

  function renderWithCompleted(extraMocks: MockedResponse[] = []) {
    const initialOpenActions = {
      request: { query: GET_OPEN_CASE_ACTIONS, variables: { workspaceId: 'risk' } },
      result: { data: { openCaseActions: [...allOpenActions, completedA, completedB] } },
    }
    const mocks = [fieldConfigMock, initialOpenActions, openCasesMock, ...extraMocks]
    return render(
      <MemoryRouter initialEntries={['/ws/risk/actions']}>
        <MockedProvider mocks={mocks}>
          <I18nProvider defaultLang="en">
            <Routes>
              <Route path="/ws/:workspaceId/actions" element={<ActionList />} />
            </Routes>
          </I18nProvider>
        </MockedProvider>
      </MemoryRouter>,
    )
  }

  it('shows the column kebab menu only on the completed column', async () => {
    renderWithCompleted()
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(5)
    })
    // The closed (COMPLETED) column exposes the menu...
    expect(screen.getByTestId('kanban-column-menu-completed')).toBeInTheDocument()
    // ...while open columns do not.
    expect(screen.queryByTestId('kanban-column-menu-to-do')).toBeNull()
    expect(screen.queryByTestId('kanban-column-menu-in-progress')).toBeNull()
    expect(screen.queryByTestId('kanban-column-menu-backlog')).toBeNull()
    expect(screen.queryByTestId('kanban-column-menu-blocked')).toBeNull()
  })

  it('archives every action in the completed column after confirmation', async () => {
    // The mutation returns the accepted ids (archiving happens async server-side).
    const bulkResult = vi.fn(() => ({
      data: { bulkArchiveActions: [201, 202] },
    }))
    const bulkArchiveMock = {
      request: { query: BULK_ARCHIVE_ACTIONS, variables: { workspaceId: 'risk', ids: [201, 202] } },
      result: bulkResult,
    }

    renderWithCompleted([bulkArchiveMock])
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(5)
    })

    fireEvent.click(screen.getByTestId('kanban-column-menu-completed'))
    fireEvent.click(screen.getByTestId('kanban-column-archive-all-completed'))

    // Confirmation dialog must appear before any mutation fires.
    const confirmBtn = await screen.findByTestId('confirm-bulk-archive-button')
    expect(bulkResult).not.toHaveBeenCalled()

    fireEvent.click(confirmBtn)

    // The mutation must run with exactly the completed column's action ids.
    await waitFor(() => {
      expect(bulkResult).toHaveBeenCalledTimes(1)
    })

    // The completed cards are optimistically removed from the board (the
    // server archives asynchronously, so the UI does not wait for it).
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(3)
    })
    expect(screen.queryByText('Finished alpha')).toBeNull()
    expect(screen.queryByText('Finished beta')).toBeNull()
  })

  it('disables the archive item when the completed column is empty', async () => {
    const emptyOpenActions = {
      request: { query: GET_OPEN_CASE_ACTIONS, variables: { workspaceId: 'risk' } },
      result: { data: { openCaseActions: allOpenActions } },
    }
    render(
      <MemoryRouter initialEntries={['/ws/risk/actions']}>
        <MockedProvider mocks={[fieldConfigMock, emptyOpenActions, openCasesMock]}>
          <I18nProvider defaultLang="en">
            <Routes>
              <Route path="/ws/:workspaceId/actions" element={<ActionList />} />
            </Routes>
          </I18nProvider>
        </MockedProvider>
      </MemoryRouter>,
    )
    await waitFor(() => {
      expect(screen.getAllByTestId('action-card')).toHaveLength(3)
    })
    fireEvent.click(screen.getByTestId('kanban-column-menu-completed'))
    const archiveItem = screen.getByTestId('kanban-column-archive-all-completed')
    expect(archiveItem).toBeDisabled()
  })
})


describe('ActionList case field filters', () => {
  it('filters actions by their parent case, including a case outside the open-case picker', async () => {
    const config = {
      ...fieldConfigMock,
      result: { data: { fieldConfiguration: {
        ...fieldConfigMock.result.data.fieldConfiguration,
        fields: [{ id: 'category', name: 'Category', type: 'SELECT', options: [{ id: 'it', name: 'IT' }, { id: 'sales', name: 'Sales' }] }],
      } } },
    }
    const rows = [
      { ...actionRow(201, 9, 'Closed IT case', 'Selected action'), case: { id: 9, title: 'Closed IT case', accessDenied: false, fields: [{ fieldId: 'category', value: 'it' }] } },
      { ...actionRow(202, 4, 'Sales', 'Excluded action'), case: { id: 4, title: 'Sales', accessDenied: false, fields: [{ fieldId: 'category', value: 'sales' }] } },
    ]
    const mocks = [config, openCasesMock, {
      request: { query: GET_ACTIONS_BY_CASE, variables: { workspaceId: 'risk', caseID: 9 } },
      result: { data: { actionsByCase: rows } },
    }]
    render(<MemoryRouter initialEntries={['/ws/risk/actions/case/9?field.category=it']}>
      <MockedProvider mocks={mocks} addTypename={false}><I18nProvider defaultLang="en">
        <Routes><Route path="/ws/:workspaceId/actions/case/:caseId" element={<ActionList />} /></Routes>
      </I18nProvider></MockedProvider>
    </MemoryRouter>)
    expect(await screen.findByText('Selected action')).toBeInTheDocument()
    expect(screen.queryByText('Excluded action')).toBeNull()
    fireEvent.click(screen.getByTestId('case-field-filters-button'))
    fireEvent.click(screen.getByTestId('case-field-filters-clear'))
    expect(await screen.findByText('Excluded action')).toBeInTheDocument()
  })
})
