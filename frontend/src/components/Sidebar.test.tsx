import { describe, expect, it, vi, afterEach } from 'vitest'
import { render, screen, waitFor, within, cleanup } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { MemoryRouter } from 'react-router'
import { I18nProvider } from '../i18n'
import { GET_CASE_STATUS_CONFIG } from '../graphql/caseStatus'
import { GET_ACTIONS_ENABLED } from '../graphql/action'
import Sidebar from './Sidebar'

vi.mock('../contexts/workspace-context', () => ({
  useWorkspace: () => ({
    currentWorkspace: { id: 'ws1', name: 'Workspace 1' },
    workspaces: [{ id: 'ws1', name: 'Workspace 1' }],
    isLoading: false,
    setCurrentWorkspace: vi.fn(),
    switchWorkspace: vi.fn(),
  }),
}))

vi.mock('../contexts/auth-context', () => ({
  useAuth: () => ({ user: null }),
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

function actionsEnabledMock(enabled: boolean): MockedResponse {
  return {
    request: { query: GET_ACTIONS_ENABLED, variables: { workspaceId: 'ws1' } },
    result: { data: { actionsEnabled: enabled } },
  }
}

function renderSidebar(mocks: MockedResponse[]) {
  return render(
    <MockedProvider mocks={mocks} addTypename={false}>
      <MemoryRouter initialEntries={['/ws/ws1/cases']}>
        <I18nProvider defaultLang="en">
          <Sidebar />
        </I18nProvider>
      </MemoryRouter>
    </MockedProvider>,
  )
}

function nav() {
  return within(screen.getByRole('navigation'))
}

afterEach(() => cleanup())

describe('Sidebar', () => {
  it('shows Actions and no Board in a channel-mode workspace', async () => {
    renderSidebar([channelModeMock, actionsEnabledMock(true)])
    const actions = await nav().findByRole('link', { name: /^Actions/ })
    expect(actions).toHaveAttribute('href', '/ws/ws1/actions')
    expect(nav().queryByRole('link', { name: /^Board/ })).toBeNull()
  })

  it('shows Board and hides Actions in a thread-mode workspace that leaves actions off', async () => {
    renderSidebar([threadModeMock, actionsEnabledMock(false)])
    const board = await nav().findByRole('link', { name: /^Board/ })
    expect(board).toHaveAttribute('href', '/ws/ws1/board')
    await waitFor(() => {
      expect(nav().queryByRole('link', { name: /^Actions/ })).toBeNull()
    })
  })

  it('shows both Board and Actions in a thread-mode workspace that enables actions', async () => {
    renderSidebar([threadModeMock, actionsEnabledMock(true)])
    const board = await nav().findByRole('link', { name: /^Board/ })
    expect(board).toHaveAttribute('href', '/ws/ws1/board')
    const actions = await nav().findByRole('link', { name: /^Actions/ })
    expect(actions).toHaveAttribute('href', '/ws/ws1/actions')
  })
})
