import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { MemoryRouter, Route, Routes } from 'react-router'
import { I18nProvider } from '../i18n'
import { GET_MEMO, GET_MEMO_CONFIGURATION, ARCHIVE_MEMO } from '../graphql/memo'
import MemoDetail from './MemoDetail'

const WS = 'risk'
const CASE_ID = 1
const MEMO_ID = 'memo-1'
const MEMO_PATH = `/ws/${WS}/cases/${CASE_ID}/memos/${MEMO_ID}`

// These tests intentionally let MockedProvider add __typename (the default) so
// Apollo's normalized cache behaves like production: a mutation that returns
// the same Memo entity updates the active GET_MEMO watcher in place. This is
// what drives the archive → archived-view transition asserted below.
function memoData(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    __typename: 'Memo',
    id: MEMO_ID,
    caseID: CASE_ID,
    title: 'Phase check 2026-07-13',
    fields: [
      { __typename: 'FieldValue', fieldId: 'severity', value: 'high', display: null },
      { __typename: 'FieldValue', fieldId: 'note', value: 'plain text note', display: null },
    ],
    archivedAt: null,
    createdAt: '2026-07-13T01:15:41Z',
    updatedAt: '2026-07-14T02:00:00Z',
    ...overrides,
  }
}

function memoMock(
  overrides: Partial<Record<string, unknown>> = {},
  caseArchivedAt: string | null = null,
): MockedResponse {
  return {
    request: {
      query: GET_MEMO,
      variables: { workspaceId: WS, caseID: CASE_ID, id: MEMO_ID },
    },
    result: {
      data: {
        memo: {
          // GET_MEMO also selects the parent case's archive state; the
          // mutations do not, so it is added here rather than in memoData.
          case: { __typename: 'Case', id: CASE_ID, workspaceId: WS, archivedAt: caseArchivedAt },
          ...memoData(overrides),
        },
      },
    },
  }
}

function memoErrorMock(): MockedResponse {
  return {
    request: {
      query: GET_MEMO,
      variables: { workspaceId: WS, caseID: CASE_ID, id: MEMO_ID },
    },
    error: new Error('memo not found'),
  }
}

function configMock(): MockedResponse {
  return {
    request: {
      query: GET_MEMO_CONFIGURATION,
      variables: { workspaceId: WS },
    },
    result: {
      data: {
        memoConfiguration: {
          __typename: 'MemoConfiguration',
          description: 'Memo config',
          fields: [
            {
              __typename: 'FieldDefinition',
              id: 'severity',
              name: 'Severity',
              type: 'SELECT',
              required: false,
              description: null,
              options: [
                { __typename: 'FieldOption', id: 'high', name: 'High', description: null, metadata: null },
                { __typename: 'FieldOption', id: 'low', name: 'Low', description: null, metadata: null },
              ],
            },
            {
              __typename: 'FieldDefinition',
              id: 'note',
              name: 'Note',
              type: 'TEXT',
              required: false,
              description: null,
              options: null,
            },
          ],
        },
      },
    },
  }
}

function configErrorMock(): MockedResponse {
  return {
    request: {
      query: GET_MEMO_CONFIGURATION,
      variables: { workspaceId: WS },
    },
    error: new Error('config unavailable'),
  }
}

function archiveMock(): MockedResponse {
  return {
    request: {
      query: ARCHIVE_MEMO,
      variables: { workspaceId: WS, caseID: CASE_ID, id: MEMO_ID },
    },
    result: {
      data: { archiveMemo: memoData({ archivedAt: '2026-07-15T00:00:00Z' }) },
    },
  }
}

function archiveErrorMock(): MockedResponse {
  return {
    request: {
      query: ARCHIVE_MEMO,
      variables: { workspaceId: WS, caseID: CASE_ID, id: MEMO_ID },
    },
    error: new Error('network down'),
  }
}

function renderAt(path: string, mocks: MockedResponse[]) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <MockedProvider mocks={mocks}>
        <I18nProvider defaultLang="en">
          <Routes>
            <Route path="/ws/:workspaceId/cases/:id/memos/:memoId" element={<MemoDetail />} />
          </Routes>
        </I18nProvider>
      </MockedProvider>
    </MemoryRouter>,
  )
}

afterEach(cleanup)

describe('MemoDetail', () => {
  it('renders title, full id, field values, and active-memo actions', async () => {
    renderAt(MEMO_PATH, [memoMock(), configMock()])

    await screen.findByTestId('memo-detail-page')

    expect(screen.getByTestId('memo-detail-title')).toHaveTextContent('Phase check 2026-07-13')
    // Full memo id (not the 8-char truncation the old modal used).
    expect(screen.getByText(MEMO_ID)).toBeInTheDocument()
    // Field labels and rendered values (SELECT resolves the option name).
    expect(screen.getByText('Severity')).toBeInTheDocument()
    expect(screen.getByText('High')).toBeInTheDocument()
    expect(screen.getByText('Note')).toBeInTheDocument()
    expect(screen.getByText('plain text note')).toBeInTheDocument()
    // Active memo: edit + archive, no unarchive, no archived badge/notice.
    expect(screen.getByTestId('memo-detail-edit-button')).toBeInTheDocument()
    expect(screen.getByTestId('memo-detail-archive-button')).toBeInTheDocument()
    expect(screen.queryByTestId('memo-detail-unarchive-button')).not.toBeInTheDocument()
    expect(screen.queryByText('Archived')).not.toBeInTheDocument()
    // Back link points at the parent case.
    expect(screen.getByTestId('memo-detail-back-link')).toHaveAttribute(
      'href',
      `/ws/${WS}/cases/${CASE_ID}`,
    )
  })

  it('renders archived state with badge, notice, and unarchive action', async () => {
    renderAt(MEMO_PATH, [memoMock({ archivedAt: '2026-07-15T00:00:00Z' }), configMock()])

    await screen.findByTestId('memo-detail-page')

    expect(screen.getByText('Archived')).toBeInTheDocument()
    expect(
      screen.getByText('This memo is archived and cannot be edited. Restore it to make it editable again.'),
    ).toBeInTheDocument()
    expect(screen.getByTestId('memo-detail-unarchive-button')).toBeInTheDocument()
    expect(screen.queryByTestId('memo-detail-edit-button')).not.toBeInTheDocument()
    expect(screen.queryByTestId('memo-detail-archive-button')).not.toBeInTheDocument()
  })

  it('hides Edit on a memo of an archived case and says why, keeping Archive', async () => {
    renderAt(MEMO_PATH, [memoMock({}, '2026-07-15T00:00:00Z'), configMock()])

    await screen.findByTestId('memo-detail-page')

    expect(screen.queryByTestId('memo-detail-edit-button')).not.toBeInTheDocument()
    expect(screen.getByTestId('memo-detail-archive-button')).toBeInTheDocument()
    expect(screen.getByTestId('memo-detail-case-archived-notice')).toHaveTextContent(
      'this memo belongs to is archived, so the memo cannot be edited.',
    )
  })

  it('shows the load-error state with retry and a back-to-case link on a memo error', async () => {
    renderAt(MEMO_PATH, [memoErrorMock(), configMock()])

    await screen.findByTestId('memo-detail-error')

    expect(screen.getByText('Failed to load memos')).toBeInTheDocument()
    expect(screen.getByText('Retry')).toBeInTheDocument()
    // A deep link that fails must still offer a way back to the case.
    expect(screen.getByTestId('memo-detail-error-back-link')).toHaveAttribute(
      'href',
      `/ws/${WS}/cases/${CASE_ID}`,
    )
    expect(screen.queryByTestId('memo-detail-page')).not.toBeInTheDocument()
    expect(screen.queryByTestId('memo-detail-title')).not.toBeInTheDocument()
  })

  it('treats a failed field-configuration load as a full-page error, not a memo with no fields', async () => {
    renderAt(MEMO_PATH, [memoMock(), configErrorMock()])

    await screen.findByTestId('memo-detail-error')

    // The memo itself loaded fine, but without the field schema we must NOT
    // silently render the memo with all field values missing.
    expect(screen.queryByTestId('memo-detail-page')).not.toBeInTheDocument()
    expect(screen.getByText('Failed to load memos')).toBeInTheDocument()
    expect(screen.getByTestId('memo-detail-error-back-link')).toBeInTheDocument()
  })

  it('archives through the confirm dialog and flips the page to the archived view', async () => {
    renderAt(MEMO_PATH, [memoMock(), configMock(), archiveMock()])

    await screen.findByTestId('memo-detail-page')

    fireEvent.click(screen.getByTestId('memo-detail-archive-button'))
    const confirm = await screen.findByTestId('memo-archive-confirm-button')
    fireEvent.click(confirm)

    // The dialog closes and the normalized-cache update flips the page in
    // place to the archived state (unarchive action + badge), without
    // navigating away.
    await waitFor(() => {
      expect(screen.queryByTestId('memo-archive-confirm-button')).not.toBeInTheDocument()
    })
    expect(await screen.findByTestId('memo-detail-unarchive-button')).toBeInTheDocument()
    expect(screen.getByText('Archived')).toBeInTheDocument()
    expect(screen.queryByTestId('memo-detail-archive-button')).not.toBeInTheDocument()
    expect(screen.queryByText('Operation failed. Please try again.')).not.toBeInTheDocument()
  })

  it('surfaces a mutation failure as an inline error message', async () => {
    renderAt(MEMO_PATH, [memoMock(), configMock(), archiveErrorMock()])

    await screen.findByTestId('memo-detail-page')

    fireEvent.click(screen.getByTestId('memo-detail-archive-button'))
    fireEvent.click(await screen.findByTestId('memo-archive-confirm-button'))

    expect(await screen.findByText('Operation failed. Please try again.')).toBeInTheDocument()
    // The dialog is closed; the page (still active) stays rendered.
    expect(screen.queryByTestId('memo-archive-confirm-button')).not.toBeInTheDocument()
    expect(screen.getByTestId('memo-detail-page')).toBeInTheDocument()
    expect(screen.getByTestId('memo-detail-archive-button')).toBeInTheDocument()
  })

  it('renders an error page with a back-to-list link for a non-numeric case id', () => {
    renderAt(`/ws/${WS}/cases/abc/memos/${MEMO_ID}`, [])

    // No blank screen: an invalid deep link is a dead end without a way out,
    // so it renders the error page with a link back to the case list.
    expect(screen.queryByTestId('memo-detail-page')).not.toBeInTheDocument()
    const back = screen.getByTestId('memo-detail-error-back-link')
    expect(back).toHaveAttribute('href', `/ws/${WS}/cases`)
  })
})
