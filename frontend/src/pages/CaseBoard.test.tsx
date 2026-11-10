import { describe, expect, it, vi, afterEach } from 'vitest'
import { render, screen, cleanup } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { MemoryRouter, Routes, Route, useLocation } from 'react-router'
import { GET_CASE_STATUS_CONFIG } from '../graphql/caseStatus'
import CaseBoard from './CaseBoard'

vi.mock('../contexts/workspace-context', () => ({
  useWorkspace: () => ({
    currentWorkspace: { id: 'ws1', name: 'Workspace 1' },
    workspaces: [{ id: 'ws1', name: 'Workspace 1' }],
    isLoading: false,
    setCurrentWorkspace: vi.fn(),
    switchWorkspace: vi.fn(),
  }),
}))

// The Kanban itself is covered elsewhere; this test is about routing.
vi.mock('./CaseKanban', () => ({
  default: () => <div data-testid="case-kanban-stub" />,
}))

const threadModeMock: MockedResponse = {
  request: { query: GET_CASE_STATUS_CONFIG, variables: { workspaceId: 'ws1' } },
  result: {
    data: {
      caseStatusConfig: {
        initial: 'open',
        closed: ['done'],
        statuses: [
          { id: 'open', name: 'Open', description: null, color: 'idle', emoji: null },
          { id: 'done', name: 'Done', description: null, color: 'success', emoji: null },
        ],
      },
    },
  },
}

const channelModeMock: MockedResponse = {
  request: { query: GET_CASE_STATUS_CONFIG, variables: { workspaceId: 'ws1' } },
  result: { data: { caseStatusConfig: null } },
}

function LocationProbe() {
  const { pathname, search } = useLocation()
  return <div data-testid="location">{pathname + search}</div>
}

function renderAt(path: string, mocks: MockedResponse[]) {
  return render(
    <MockedProvider mocks={mocks} addTypename={false}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/ws/:workspaceId/board" element={<CaseBoard />} />
          <Route path="/ws/:workspaceId/actions" element={<LocationProbe />} />
        </Routes>
      </MemoryRouter>
    </MockedProvider>,
  )
}

afterEach(() => cleanup())

describe('CaseBoard', () => {
  it('renders the Case Kanban in a thread-mode workspace', async () => {
    renderAt('/ws/ws1/board', [threadModeMock])
    expect(await screen.findByTestId('case-kanban-stub')).toBeInTheDocument()
  })

  it('forwards a channel-mode workspace to its Action board, keeping the query', async () => {
    renderAt('/ws/ws1/board?field.category=bug', [channelModeMock])
    expect(await screen.findByTestId('location')).toHaveTextContent('/ws/ws1/actions?field.category=bug')
    expect(screen.queryByTestId('case-kanban-stub')).toBeNull()
  })
})
