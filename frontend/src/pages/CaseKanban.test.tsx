import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider } from '@apollo/client/testing'
import { MemoryRouter, useLocation } from 'react-router'
import { I18nProvider } from '../i18n'
import { GET_CASES } from '../graphql/case'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'
import CaseKanban from './CaseKanban'

vi.mock('../contexts/workspace-context', () => ({ useWorkspace: () => ({ currentWorkspace: { id: 'review', name: 'Review' } }) }))
vi.mock('../hooks/useCaseStatuses', () => ({ useCaseStatuses: () => ({
  statuses: [{ id: 'triage', name: 'Triage' }, { id: 'done', name: 'Done' }],
  isClosed: (id: string) => id === 'done', label: (id: string) => id,
}) }))
afterEach(cleanup)
function Location() { return <span data-testid="query">{useLocation().search}</span> }
function setup(path: string) {
  const mocks = [
    { request: { query: GET_CASES, variables: { workspaceId: 'review' } }, result: { data: { cases: [
      { id: 1, title: 'IT case', boardStatus: 'triage', fields: [{ fieldId: 'category', value: 'it' }] },
      { id: 2, title: 'Sales case', boardStatus: 'triage', fields: [{ fieldId: 'category', value: 'sales' }] },
      { id: 3, title: 'Closed IT case', boardStatus: 'done', fields: [{ fieldId: 'category', value: 'it' }] },
    ].map((c) => ({ ...c, __typename: 'Case', workspaceId: 'review', description: '', status: 'OPEN', archivedAt: null, isPrivate: false, isTest: false, accessDenied: false, reporterID: null, reporter: null, assigneeIDs: [], assignees: [], slackChannelID: null, slackThreadTS: null, isThreadBound: true, createdAt: '', updatedAt: '', fields: c.fields.map((f) => ({ ...f, display: null })) })) } } },
    { request: { query: GET_FIELD_CONFIGURATION, variables: { workspaceId: 'review' } }, result: { data: { fieldConfiguration: {
      labels: { case: 'Case' }, actionConfig: { initial: '', closed: [], statuses: [] }, allowExistingSlackChannel: false,
      fields: [{ id: 'category', name: 'Category', type: 'SELECT', required: false, description: null, referenceWorkspaceId: null, options: [{ id: 'it', name: 'IT', description: null, metadata: null }, { id: 'sales', name: 'Sales', description: null, metadata: null }] }],
    } } } },
  ]
  render(<MemoryRouter initialEntries={[path]}><MockedProvider mocks={mocks} addTypename={false}>
    <I18nProvider defaultLang="en"><CaseKanban /><Location /></I18nProvider>
  </MockedProvider></MemoryRouter>)
}

describe('CaseKanban field filters', () => {
  it('restores a shared URL, groups only matching cards and updates counts', async () => {
    setup('/ws/review/actions?field.category=it')
    expect(await screen.findByText('#1 IT case')).toBeInTheDocument()
    expect(screen.getByText('#3 Closed IT case')).toBeInTheDocument()
    expect(screen.queryByText('#2 Sales case')).toBeNull()
    expect(screen.getByText(/1 open/)).toBeInTheDocument()
    fireEvent.change(screen.getByTestId('case-board-search-input'), { target: { value: 'Closed' } })
    expect(screen.queryByText('#1 IT case')).toBeNull()
    expect(screen.getByText('#3 Closed IT case')).toBeInTheDocument()
  })

  it('selects multiple categories and clears the URL without dropping other parameters', async () => {
    setup('/ws/review/actions?custom=keep&field.category=it')
    await screen.findByText('#1 IT case')
    fireEvent.click(screen.getByTestId('case-field-filters-button'))
    fireEvent.keyDown(screen.getByRole('combobox', { name: 'Category' }), { key: 'ArrowDown' })
    fireEvent.click(screen.getByRole('option', { name: 'Sales' }))
    expect(await screen.findByText('#2 Sales case')).toBeInTheDocument()
    expect(screen.getByTestId('query')).toHaveTextContent('?custom=keep&field.category=it&field.category=sales')
    fireEvent.click(screen.getByTestId('case-field-filters-clear'))
    expect(screen.getByTestId('query')).toHaveTextContent('?custom=keep')
  })
})
