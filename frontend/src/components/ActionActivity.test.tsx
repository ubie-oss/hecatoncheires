import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { I18nProvider } from '../i18n'
import {
  DELETE_ACTION_COMMENT,
  GET_ACTION_COMMENTS,
  GET_ACTION_EVENTS,
  GET_ACTION_MESSAGES,
  UPDATE_ACTION_COMMENT,
} from '../graphql/action'
import { GET_SLACK_USERS } from '../graphql/slackUsers'
import ActionActivity from './ActionActivity'

vi.mock('../contexts/auth-context', () => ({
  useAuth: () => ({
    user: { sub: 'UME', email: 'me@example.test', name: 'Me' },
    isLoading: false,
    isAuthenticated: true,
    login: vi.fn(),
    logout: vi.fn(),
    checkAuth: vi.fn(),
  }),
}))

vi.mock('../hooks/useActionStatuses', () => ({
  useActionStatuses: () => ({
    statuses: [],
    label: (id: string) => id,
    get: () => undefined,
  }),
}))

const WS = 'risk'
const ACTION_ID = 7
const PAGE_SIZE = 20

function messagesMock(): MockedResponse {
  return {
    request: {
      query: GET_ACTION_MESSAGES,
      variables: { workspaceId: WS, id: ACTION_ID, limit: PAGE_SIZE, cursor: null },
    },
    result: {
      data: {
        action: {
          id: ACTION_ID,
          workspaceId: WS,
          messages: {
            items: [
              {
                id: 'msg-1',
                channelID: 'C1',
                threadTS: 'parent',
                teamID: 'T1',
                userID: 'UOTHER',
                userName: 'bob',
                text: 'slack reply text',
                createdAt: '2026-08-20T02:00:00Z',
                files: [],
              },
            ],
            nextCursor: '',
          },
        },
      },
    },
  }
}

function eventsMock(): MockedResponse {
  return {
    request: {
      query: GET_ACTION_EVENTS,
      variables: { workspaceId: WS, id: ACTION_ID, limit: PAGE_SIZE, cursor: null },
    },
    result: {
      data: {
        action: {
          id: ACTION_ID,
          workspaceId: WS,
          events: {
            items: [
              {
                id: 'event-1',
                actionID: ACTION_ID,
                kind: 'STATUS_CHANGED',
                actorID: 'UOTHER',
                actor: null,
                oldValue: 'TODO',
                newValue: 'IN_PROGRESS',
                createdAt: '2026-08-20T01:00:00Z',
              },
            ],
            nextCursor: '',
          },
        },
      },
    },
  }
}

function comment(id: string, authorID: string, body: string, createdAt: string, edited = false) {
  return {
    id,
    actionID: ACTION_ID,
    authorID,
    body,
    createdAt,
    updatedAt: edited ? '2026-08-20T09:00:00Z' : createdAt,
    edited,
    author: null,
  }
}

function commentsMock(limit: number, items: ReturnType<typeof comment>[]): MockedResponse {
  return {
    request: {
      query: GET_ACTION_COMMENTS,
      variables: { workspaceId: WS, id: ACTION_ID, limit, cursor: null },
    },
    result: {
      data: {
        action: {
          id: ACTION_ID,
          workspaceId: WS,
          comments: { items, nextCursor: '' },
        },
      },
    },
    maxUsageCount: 5,
  }
}

function updateMock(commentId: string, body: string, fail = false): MockedResponse {
  const base = {
    request: {
      query: UPDATE_ACTION_COMMENT,
      variables: { workspaceId: WS, input: { actionId: ACTION_ID, commentId, body } },
    },
  }
  if (fail) return { ...base, error: new Error('boom') }
  return {
    ...base,
    result: {
      data: {
        updateActionComment: {
          id: commentId,
          actionID: ACTION_ID,
          authorID: 'UME',
          body,
          createdAt: '2026-08-20T03:00:00Z',
          updatedAt: '2026-08-20T09:00:00Z',
          edited: true,
          author: null,
        },
      },
    },
  }
}

function deleteMock(commentId: string, fail = false): MockedResponse {
  const base = {
    request: {
      query: DELETE_ACTION_COMMENT,
      variables: { workspaceId: WS, input: { actionId: ACTION_ID, commentId } },
    },
  }
  if (fail) return { ...base, error: new Error('boom') }
  return { ...base, result: { data: { deleteActionComment: true } } }
}

function usersMock(): MockedResponse {
  return {
    request: { query: GET_SLACK_USERS },
    result: {
      data: {
        slackUsers: [
          { id: 'UME', name: 'me', realName: 'Me', imageUrl: '' },
          { id: 'UOTHER', name: 'bob', realName: 'Bob', imageUrl: '' },
        ],
      },
    },
    maxUsageCount: 5,
  }
}

function renderActivity(mocks: MockedResponse[], highlightCommentId?: string, readOnly = false) {
  return render(
    <MockedProvider mocks={mocks} addTypename={false}>
      <I18nProvider>
        <ActionActivity
          workspaceId={WS}
          actionId={ACTION_ID}
          pageSize={PAGE_SIZE}
          highlightCommentId={highlightCommentId}
          readOnly={readOnly}
        />
      </I18nProvider>
    </MockedProvider>,
  )
}

describe('ActionActivity comments', () => {
  afterEach(cleanup)

  it('renders comments and Slack messages together, newest first', async () => {
    const mine = comment('c-mine', 'UME', 'my comment', '2026-08-20T03:00:00Z')
    renderActivity([messagesMock(), eventsMock(), commentsMock(PAGE_SIZE, [mine]), usersMock()])

    await screen.findByTestId('activity-comment-c-mine')
    expect(screen.getByText('my comment')).toBeInTheDocument()
    expect(screen.getByText('slack reply text')).toBeInTheDocument()

    // The comment (03:00) is newer than the Slack reply (02:00), so it sorts first.
    const feed = screen.getByTestId('action-activity')
    const rendered = feed.textContent ?? ''
    expect(rendered.indexOf('my comment')).toBeLessThan(rendered.indexOf('slack reply text'))
  })

  it('hides comments on the History tab and keeps them on the Comments tab', async () => {
    const mine = comment('c-mine', 'UME', 'my comment', '2026-08-20T03:00:00Z')
    renderActivity([messagesMock(), eventsMock(), commentsMock(PAGE_SIZE, [mine]), usersMock()])

    await screen.findByTestId('activity-comment-c-mine')

    screen.getByTestId('activity-tab-history').click()
    await waitFor(() => expect(screen.queryByTestId('activity-comment-c-mine')).toBeNull())
    expect(screen.getByTestId('activity-event-status_changed')).toBeInTheDocument()

    screen.getByTestId('activity-tab-comments').click()
    await screen.findByTestId('activity-comment-c-mine')
    expect(screen.queryByTestId('activity-event-status_changed')).toBeNull()
  })

  it('offers edit and delete only on the signed-in user\'s own comment', async () => {
    const mine = comment('c-mine', 'UME', 'my comment', '2026-08-20T03:00:00Z')
    const theirs = comment('c-theirs', 'UOTHER', 'their comment', '2026-08-20T04:00:00Z')
    renderActivity([messagesMock(), eventsMock(), commentsMock(PAGE_SIZE, [theirs, mine]), usersMock()])

    await screen.findByTestId('activity-comment-c-mine')
    expect(screen.getByTestId('action-comment-edit-c-mine')).toBeInTheDocument()
    expect(screen.getByTestId('action-comment-delete-c-mine')).toBeInTheDocument()
    expect(screen.getByTestId('action-comment-composer')).toBeInTheDocument()
    expect(screen.queryByTestId('action-comment-edit-c-theirs')).toBeNull()
    expect(screen.queryByTestId('action-comment-delete-c-theirs')).toBeNull()
  })

  it('offers no comment write control when read-only', async () => {
    const mine = comment('c-mine', 'UME', 'my comment', '2026-08-20T03:00:00Z')
    renderActivity([messagesMock(), eventsMock(), commentsMock(PAGE_SIZE, [mine]), usersMock()], undefined, true)

    await screen.findByTestId('activity-comment-c-mine')
    // The comment itself is still shown.
    expect(screen.getByText('my comment')).toBeInTheDocument()
    expect(screen.queryByTestId('action-comment-edit-c-mine')).toBeNull()
    expect(screen.queryByTestId('action-comment-delete-c-mine')).toBeNull()
    expect(screen.queryByTestId('action-comment-composer')).toBeNull()
  })

  it('marks an edited comment', async () => {
    const edited = comment('c-edited', 'UME', 'revised body', '2026-08-20T03:00:00Z', true)
    renderActivity([messagesMock(), eventsMock(), commentsMock(PAGE_SIZE, [edited]), usersMock()])

    await screen.findByTestId('activity-comment-c-edited')
    expect(screen.getByText('edited')).toBeInTheDocument()
  })

  it('opens the Comments tab and queries a larger page when deep-linked', async () => {
    const target = comment('c-target', 'UOTHER', 'the linked comment', '2026-08-20T03:00:00Z')
    renderActivity(
      [messagesMock(), eventsMock(), commentsMock(100, [target]), usersMock()],
      'c-target',
    )

    // The Comments tab is selected, so the STATUS_CHANGED event is not rendered.
    await screen.findByTestId('activity-comment-c-target')
    expect(screen.getByTestId('activity-tab-comments')).toHaveAttribute('aria-selected', 'true')
    expect(screen.queryByTestId('activity-event-status_changed')).toBeNull()
  })

  it('renders without highlighting when the deep-linked comment is not in the page', async () => {
    const other = comment('c-other', 'UOTHER', 'unrelated', '2026-08-20T03:00:00Z')
    renderActivity(
      [messagesMock(), eventsMock(), commentsMock(100, [other]), usersMock()],
      'c-missing',
    )

    await screen.findByTestId('activity-comment-c-other')
    expect(screen.queryByTestId('activity-comment-c-missing')).toBeNull()
  })

  it('sends the edited body and closes the editor on success', async () => {
    const mine = comment('c-mine', 'UME', 'first draft', '2026-08-20T03:00:00Z')
    renderActivity([
      messagesMock(),
      eventsMock(),
      commentsMock(PAGE_SIZE, [mine]),
      usersMock(),
      updateMock('c-mine', 'second draft'),
    ])

    await screen.findByTestId('activity-comment-c-mine')
    fireEvent.click(screen.getByTestId('action-comment-edit-c-mine'))

    const textarea = (await screen.findByTestId('action-comment-editor-c-mine-textarea')) as HTMLTextAreaElement
    fireEvent.change(textarea, { target: { value: 'second draft' } })
    fireEvent.click(screen.getByTestId('action-comment-save-c-mine'))

    // The editor closes only when the mutation resolved with the variables the
    // mock declared; a mismatch leaves it open with an error instead.
    await waitFor(() => expect(screen.queryByTestId('action-comment-editor-c-mine-textarea')).toBeNull())
    expect(screen.queryByTestId('action-comment-update-error-c-mine')).toBeNull()
  })

  it('keeps the editor open with an error when saving fails', async () => {
    const mine = comment('c-mine', 'UME', 'first draft', '2026-08-20T03:00:00Z')
    renderActivity([
      messagesMock(),
      eventsMock(),
      commentsMock(PAGE_SIZE, [mine]),
      usersMock(),
      updateMock('c-mine', 'doomed edit', true),
    ])

    await screen.findByTestId('activity-comment-c-mine')
    fireEvent.click(screen.getByTestId('action-comment-edit-c-mine'))

    const textarea = (await screen.findByTestId('action-comment-editor-c-mine-textarea')) as HTMLTextAreaElement
    fireEvent.change(textarea, { target: { value: 'doomed edit' } })
    fireEvent.click(screen.getByTestId('action-comment-save-c-mine'))

    await screen.findByTestId('action-comment-update-error-c-mine')
    expect(screen.getByTestId('action-comment-editor-c-mine-textarea')).toHaveValue('doomed edit')
  })

  it('does not send an update when the body is unchanged', async () => {
    const mine = comment('c-mine', 'UME', 'unchanged', '2026-08-20T03:00:00Z')
    // No update mock: a mutation here would fail the test with "No more mocked
    // responses", which is exactly the regression we want to catch.
    renderActivity([messagesMock(), eventsMock(), commentsMock(PAGE_SIZE, [mine]), usersMock()])

    await screen.findByTestId('activity-comment-c-mine')
    fireEvent.click(screen.getByTestId('action-comment-edit-c-mine'))
    await screen.findByTestId('action-comment-editor-c-mine-textarea')
    fireEvent.click(screen.getByTestId('action-comment-save-c-mine'))

    await waitFor(() => expect(screen.queryByTestId('action-comment-editor-c-mine-textarea')).toBeNull())
    expect(screen.queryByTestId('action-comment-update-error-c-mine')).toBeNull()
  })

  it('deletes through the confirm dialog and reports a failed delete', async () => {
    const mine = comment('c-mine', 'UME', 'to be removed', '2026-08-20T03:00:00Z')
    renderActivity([
      messagesMock(),
      eventsMock(),
      commentsMock(PAGE_SIZE, [mine]),
      usersMock(),
      deleteMock('c-mine', true),
      deleteMock('c-mine'),
    ])

    await screen.findByTestId('activity-comment-c-mine')
    fireEvent.click(screen.getByTestId('action-comment-delete-c-mine'))

    // First attempt fails: the dialog stays open and says so.
    fireEvent.click(await screen.findByTestId('action-comment-delete-confirm'))
    await screen.findByTestId('action-comment-delete-error')

    // Second attempt succeeds and the dialog closes.
    fireEvent.click(screen.getByTestId('action-comment-delete-confirm'))
    await waitFor(() => expect(screen.queryByTestId('action-comment-delete-confirm')).toBeNull())
  })
})
