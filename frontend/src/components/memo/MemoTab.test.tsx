import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { MemoryRouter, useLocation } from 'react-router'
import { I18nProvider } from '../../i18n'
import { GET_MEMOS_BY_CASE, GET_MEMO_CONFIGURATION } from '../../graphql/memo'
import MemoTab from './MemoTab'

const WS = 'risk'
const CASE_ID = 7

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
          description: '',
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
              ],
            },
          ],
        },
      },
    },
  }
}

function memoRow(id: string, title: string, archivedAt: string | null) {
  return {
    __typename: 'Memo',
    id,
    caseID: CASE_ID,
    title,
    fields:
      archivedAt === null
        ? [{ __typename: 'FieldValue', fieldId: 'severity', value: 'high', display: null }]
        : [],
    archivedAt,
    createdAt: '2026-07-13T01:00:00Z',
    updatedAt: '2026-07-13T02:00:00Z',
  }
}

function memosMock(filter: 'ACTIVE' | 'ARCHIVED'): MockedResponse {
  const rows =
    filter === 'ACTIVE'
      ? [memoRow('memo-a', 'First memo', null), memoRow('memo-b', 'Second memo', null)]
      : [memoRow('memo-c', 'Archived memo', '2026-07-15T00:00:00Z')]
  return {
    request: {
      query: GET_MEMOS_BY_CASE,
      variables: { workspaceId: WS, caseID: CASE_ID, filter },
    },
    result: { data: { memosByCase: rows } },
  }
}

interface LocationProbeRef {
  path: string
  state: unknown
}

function LocationProbe({ target }: { target: LocationProbeRef }) {
  const loc = useLocation()
  target.path = loc.pathname
  target.state = loc.state
  return null
}

function renderTab(
  props: { accessDenied?: boolean; readOnly?: boolean } = {},
  entry: { pathname: string; state?: unknown } = { pathname: `/ws/${WS}/cases/${CASE_ID}` },
  extraMocks: MockedResponse[] = [],
) {
  const probeRef: LocationProbeRef = { path: '', state: null }
  const utils = render(
    <MemoryRouter initialEntries={[entry]}>
      <MockedProvider mocks={[configMock(), memosMock('ACTIVE'), ...extraMocks]}>
        <I18nProvider defaultLang="en">
          <MemoTab caseId={CASE_ID} workspaceId={WS} accessDenied={props.accessDenied} readOnly={props.readOnly} />
          <LocationProbe target={probeRef} />
        </I18nProvider>
      </MockedProvider>
    </MemoryRouter>,
  )
  return { ...utils, probeRef }
}

afterEach(cleanup)

describe('MemoTab', () => {
  it('renders memo rows with their titles and summary chips', async () => {
    renderTab()

    const rows = await screen.findAllByTestId('memo-row')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('First memo')
    // Summary chip resolves the SELECT option name.
    expect(rows[0]).toHaveTextContent('High')
    expect(rows[1]).toHaveTextContent('Second memo')
  })

  it('renders each row as a link to the memo page (supports open-in-new-tab)', async () => {
    renderTab()

    const rows = await screen.findAllByTestId('memo-row')
    // A real anchor with an href, not a button, so Cmd/Ctrl-click and
    // copy-link work.
    expect(rows[0].tagName).toBe('A')
    expect(rows[0]).toHaveAttribute('href', `/ws/${WS}/cases/${CASE_ID}/memos/memo-a`)
  })

  it('navigates to the memo page carrying the active filter in location state', async () => {
    const { probeRef } = renderTab()

    const rows = await screen.findAllByTestId('memo-row')
    fireEvent.click(rows[0])

    expect(probeRef.path).toBe(`/ws/${WS}/cases/${CASE_ID}/memos/memo-a`)
    // The filter is echoed so returning to the case restores this view.
    expect((probeRef.state as { memoFilter?: string }).memoFilter).toBe('ACTIVE')
  })

  it('restores the archived filter from incoming location state', async () => {
    renderTab(
      {},
      { pathname: `/ws/${WS}/cases/${CASE_ID}`, state: { memoFilter: 'ARCHIVED' } },
      [memosMock('ARCHIVED')],
    )

    // The Archived tab is pre-selected (not the default Active).
    const archivedTab = await screen.findByTestId('memo-filter-archived')
    expect(archivedTab).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByTestId('memo-filter-active')).toHaveAttribute('aria-selected', 'false')
    expect(await screen.findByText('Archived memo')).toBeInTheDocument()
  })

  it('offers New memo normally and hides it when read-only, still listing memos', async () => {
    renderTab()
    await screen.findAllByTestId('memo-row')
    expect(screen.getByTestId('new-memo-button')).toBeInTheDocument()
    cleanup()

    renderTab({ readOnly: true })
    const rows = await screen.findAllByTestId('memo-row')
    expect(rows).toHaveLength(2)
    expect(screen.queryByTestId('new-memo-button')).not.toBeInTheDocument()
  })

  it('renders nothing when access is denied', () => {
    const { container } = renderTab({ accessDenied: true })

    expect(container.textContent).toBe('')
    expect(screen.queryByTestId('memo-row')).not.toBeInTheDocument()
  })
})
