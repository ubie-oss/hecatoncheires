import { useEffect, useMemo, useRef, useState, type CSSProperties } from 'react'
import { useQuery, useMutation } from '@apollo/client'
import {
  GET_ACTION_MESSAGES,
  GET_ACTION_EVENTS,
  GET_ACTION_COMMENTS,
  UPDATE_ACTION_COMMENT,
  DELETE_ACTION_COMMENT,
  POST_ACTION_SLACK_MESSAGE,
  GET_ACTION,
} from '../graphql/action'
import { GET_SLACK_USERS } from '../graphql/slackUsers'
import { useTranslation, type MsgKey } from '../i18n'
import { useAuth } from '../contexts/auth-context'
import { useActionStatuses } from '../hooks/useActionStatuses'
import { actionStatusColorStyle } from '../utils/actionStatusStyle'
import { buildSlackPermalink } from '../utils/slackLink'
import { displayName } from '../utils/user'
import Button from './Button'
import Modal from './Modal'
import MarkdownContent from './markdown/MarkdownContent'
import MarkdownEditor from './markdown/MarkdownEditor'
import ActionCommentComposer from './ActionCommentComposer'

type EventKind =
  | 'CREATED'
  | 'TITLE_CHANGED'
  | 'STATUS_CHANGED'
  | 'ASSIGNEE_CHANGED'

interface SlackUser {
  id: string
  name: string
  realName: string
  imageUrl: string
}

interface SlackMessage {
  id: string
  channelID: string
  threadTS?: string | null
  teamID: string
  userID: string
  userName: string
  text: string
  createdAt: string
}

interface ActionEvent {
  id: string
  actionID: number
  kind: EventKind
  actorID: string
  actor?: SlackUser | null
  oldValue: string
  newValue: string
  createdAt: string
}

interface ActionComment {
  id: string
  actionID: number
  authorID: string
  author?: SlackUser | null
  body: string
  createdAt: string
  updatedAt: string
  edited: boolean
}

interface MessagesData {
  action: {
    id: number
    messages: { items: SlackMessage[]; nextCursor: string }
  } | null
}

interface EventsData {
  action: {
    id: number
    events: { items: ActionEvent[]; nextCursor: string }
  } | null
}

interface CommentsData {
  action: {
    id: number
    comments: { items: ActionComment[]; nextCursor: string }
  } | null
}

interface ActionActivityProps {
  workspaceId: string
  actionId: number
  pageSize?: number
  slackMessageTS?: string | null
  slackChannelID?: string | null
  slackChannelURL?: string | null
  /** Root timestamp of the parent case's thread, set for a thread-mode case.
   * The action's Slack message is then a reply inside that thread. */
  slackThreadTS?: string | null
  /** Comment id to select the Comments tab for and scroll into view. Supplied
   * by the surrounding page from the `?comment=` deep link the Slack
   * notification carries. */
  highlightCommentId?: string | null
}

// Deep-linking to one comment must be able to reach it, so a deep-linked feed
// asks for a larger first page than the incremental one. A comment older than
// this is still reachable through "Load more".
const DEEP_LINK_COMMENT_LIMIT = 100

type Tab = 'all' | 'comments' | 'history'

const EMOJI_MAP: Record<string, string> = {
  pencil2: '✏️',
  arrows_counterclockwise: '🔁',
  bust_in_silhouette: '👤',
  link: '🔗',
  white_check_mark: '✅',
  warning: '⚠️',
  speech_balloon: '💬',
}

function renderEmojiTokens(text: string): string {
  return text.replace(/:([a-z0-9_+-]+):/gi, (m, name) => EMOJI_MAP[name.toLowerCase()] ?? m)
}

function renderText(text: string, userIdToName: Map<string, string>): string {
  let out = renderEmojiTokens(text)
  out = out.replace(/<@([A-Z0-9]+)(\|[^>]+)?>/g, (_m, uid: string) => `@${userIdToName.get(uid) ?? uid}`)
  out = out.replace(/<([^|>]+)\|([^>]+)>/g, (_m, _url, label) => label)
  out = out.replace(/<([^|>]+)>/g, (_m, url) => url)
  out = out.replace(/&gt;/g, '>').replace(/&lt;/g, '<').replace(/&amp;/g, '&')
  return out
}

function isSameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate()
}

function formatTimestamp(iso: string, todayLabel: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const now = new Date()
  const hh = String(d.getHours()).padStart(2, '0')
  const mi = String(d.getMinutes()).padStart(2, '0')
  if (isSameDay(d, now)) return `${todayLabel} ${hh}:${mi}`
  const sameYear = d.getFullYear() === now.getFullYear()
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  if (sameYear) return `${mm}/${dd} ${hh}:${mi}`
  return `${d.getFullYear()}/${mm}/${dd}`
}

function formatTimestampShort(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const now = new Date()
  const hh = String(d.getHours()).padStart(2, '0')
  const mi = String(d.getMinutes()).padStart(2, '0')
  if (isSameDay(d, now)) return `${hh}:${mi}`
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return `${mm}/${dd} ${hh}:${mi}`
}

function formatTimestampFull(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const yyyy = d.getFullYear()
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  const hh = String(d.getHours()).padStart(2, '0')
  const mi = String(d.getMinutes()).padStart(2, '0')
  return `${yyyy}-${mm}-${dd} ${hh}:${mi}`
}


const EVENT_KEY_MAP: Record<EventKind, MsgKey> = {
  CREATED: 'activityEventCreated',
  TITLE_CHANGED: 'activityEventTitleChanged',
  STATUS_CHANGED: 'activityEventStatusChanged',
  ASSIGNEE_CHANGED: 'activityEventAssigneeChanged',
}

const EVENT_ICON: Record<EventKind, string> = {
  CREATED: '＋',
  TITLE_CHANGED: '✎',
  STATUS_CHANGED: '◉',
  ASSIGNEE_CHANGED: '👤',
}

interface UserIndex {
  byName: Map<string, string>
  byImage: Map<string, string>
  byInitial: Map<string, string>
}

function initialOf(name: string): string {
  const trimmed = name.trim()
  if (!trimmed) return '?'
  return trimmed.charAt(0).toUpperCase()
}

const styles: Record<string, CSSProperties> = {
  root: { display: 'flex', flexDirection: 'column', gap: 12 },
  header: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12 },
  title: { display: 'flex', alignItems: 'baseline', gap: 6, fontSize: 13, color: 'var(--text-muted)', fontWeight: 500 },
  count: { color: 'var(--text-muted)', fontWeight: 500 },
  tabs: { display: 'inline-flex', background: 'var(--bg-subtle, #F3F4F6)', borderRadius: 6, padding: 2, gap: 2 },
  tab: { appearance: 'none', background: 'transparent', border: 'none', cursor: 'pointer', padding: '4px 10px', fontSize: 12, color: 'var(--text-muted)', borderRadius: 4, fontWeight: 500, lineHeight: 1.4 },
  tabActive: { background: 'var(--bg-paper, #FFFFFF)', color: 'var(--text-heading)', fontWeight: 600, boxShadow: '0 1px 2px rgba(0,0,0,0.05)' },
  feed: { display: 'flex', flexDirection: 'column', position: 'relative', gap: 6 },
  rail: { position: 'absolute', left: 15, top: 12, bottom: 12, width: 1, background: 'var(--border-light, #E5E7EB)' },
  row: { display: 'flex', alignItems: 'flex-start', gap: 12, position: 'relative', padding: '4px 0' },
  avatar: { flex: '0 0 auto', width: 32, height: 32, borderRadius: '50%', background: 'var(--bg-muted, #E5E7EB)', display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--text-muted)', fontSize: 12, fontWeight: 600, overflow: 'hidden', position: 'relative', zIndex: 1 },
  avatarImg: { width: '100%', height: '100%', objectFit: 'cover' },
  eventIcon: { flex: '0 0 auto', width: 32, height: 32, borderRadius: '50%', background: 'var(--bg-paper, #fff)', border: '1px solid var(--border-light, #E5E7EB)', display: 'flex', alignItems: 'center', justifyContent: 'center', color: 'var(--text-muted)', fontSize: 13, position: 'relative', zIndex: 1 },
  body: { flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column', gap: 6, paddingTop: 4 },
  messageHead: { display: 'flex', alignItems: 'baseline', gap: 8, fontSize: 13 },
  messageName: { color: 'var(--text-heading)', fontWeight: 600 },
  timestamp: { color: 'var(--text-muted)', fontSize: 12 },
  messageCard: { border: '1px solid var(--border-light, #E5E7EB)', borderRadius: 6, padding: '8px 12px', background: 'var(--bg-paper, #fff)', fontSize: 13, lineHeight: 1.6, whiteSpace: 'pre-wrap', color: 'var(--text-body)' },
  eventLine: { display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap', fontSize: 13, color: 'var(--text-muted)', minHeight: 32, paddingTop: 6, flex: 1, minWidth: 0 },
  eventActor: { color: 'var(--text-heading)', fontWeight: 600 },
  eventVerb: { color: 'var(--text-body)' },
  eventTime: { marginLeft: 'auto', color: 'var(--text-muted)', fontSize: 12, whiteSpace: 'nowrap' },
  statusPill: { display: 'inline-flex', alignItems: 'center', gap: 4, padding: '1px 6px', borderRadius: 4, fontSize: 11, fontWeight: 600, background: 'var(--bg-subtle, #F3F4F6)', color: 'var(--text-body)', border: '1px solid var(--border-light, #E5E7EB)', textTransform: 'uppercase', letterSpacing: '0.02em', fontFamily: 'var(--font-mono, ui-monospace, monospace)' },
  statusDot: { width: 6, height: 6, borderRadius: '50%', flex: '0 0 auto' },
  userPill: { display: 'inline-flex', alignItems: 'center', gap: 4, padding: '1px 6px 1px 2px', borderRadius: 999, fontSize: 12, background: 'var(--bg-subtle, #F3F4F6)', border: '1px solid var(--border-light, #E5E7EB)', color: 'var(--text-body)' },
  userPillImg: { width: 16, height: 16, borderRadius: '50%' },
  userPillFallback: { width: 16, height: 16, borderRadius: '50%', background: 'var(--bg-muted, #E5E7EB)', display: 'inline-flex', alignItems: 'center', justifyContent: 'center', fontSize: 10, color: 'var(--text-muted)' },
  titleChip: { display: 'inline-flex', alignItems: 'center', padding: '1px 6px', borderRadius: 4, fontSize: 12, background: 'var(--bg-subtle, #F3F4F6)', border: '1px solid var(--border-light, #E5E7EB)', color: 'var(--text-muted)', maxWidth: '16rem', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' },
  titleChipNew: { background: 'var(--bg-paper, #fff)', color: 'var(--text-body)', borderColor: 'var(--border-medium, #D1D5DB)' },
  arrow: { color: 'var(--text-muted)', fontSize: 12 },
  empty: { color: 'var(--text-muted)', fontSize: 12, padding: '8px 0' },
  loadMoreBar: { display: 'flex', gap: 8, alignSelf: 'flex-start' },
  slackLinkRow: { display: 'flex', justifyContent: 'flex-end', alignItems: 'center', gap: 8, paddingTop: 4 },
  slackLink: { display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 12, color: 'var(--accent)', textDecoration: 'none', padding: '4px 10px', borderRadius: 6, border: '1px solid color-mix(in oklch, var(--accent) 25%, var(--line))', background: 'color-mix(in oklch, var(--accent) 6%, transparent)' },
  slackPostButton: { display: 'inline-flex', alignItems: 'center', gap: 6, fontSize: 12, color: 'var(--accent)', cursor: 'pointer', padding: '4px 10px', borderRadius: 6, border: '1px solid color-mix(in oklch, var(--accent) 25%, var(--line))', background: 'color-mix(in oklch, var(--accent) 6%, transparent)' },
  slackPostError: { fontSize: 12, color: 'var(--color-error)' },
  inline: { display: 'inline-flex', gap: 6, alignItems: 'center', flexWrap: 'wrap' },
  commentCard: { border: '1px solid var(--border-light, #E5E7EB)', borderRadius: 6, padding: '8px 12px', background: 'var(--bg-paper, #fff)', fontSize: 13, lineHeight: 1.6, color: 'var(--text-body)' },
  commentCardHighlighted: { outline: '2px solid var(--accent)', outlineOffset: 2 },
  commentEdited: { color: 'var(--text-muted)', fontSize: 12, fontStyle: 'italic' },
  commentActions: { marginLeft: 'auto', display: 'inline-flex', gap: 4 },
  commentActionButton: { appearance: 'none', background: 'transparent', border: 'none', cursor: 'pointer', padding: '2px 6px', borderRadius: 4, fontSize: 12, color: 'var(--text-muted)', lineHeight: 1.4 },
  commentEditFooter: { display: 'flex', gap: 8, alignItems: 'center', paddingTop: 6 },
  commentError: { fontSize: 12, color: 'var(--color-error)' },
  composer: { paddingTop: 8, borderTop: '1px solid var(--border-light, #E5E7EB)' },
}

export default function ActionActivity({ workspaceId, actionId, pageSize = 20, slackMessageTS, slackChannelID, slackChannelURL, slackThreadTS, highlightCommentId }: ActionActivityProps) {
  // In a thread-mode case the action's Slack message is a reply in the case
  // thread, so the link names that thread to open the reply in place.
  const slackPermalink = buildSlackPermalink(slackChannelURL, slackChannelID, slackMessageTS, slackThreadTS)
  const { t } = useTranslation()
  const { user } = useAuth()

  // Self-repair entry point for actions whose initial Slack post never
  // happened (e.g. tool-created actions before the create paths were
  // unified). The button is shown in place of the "reply in Slack thread"
  // link when slackMessageTS is empty; clicking it triggers the unified
  // post path on the backend, which writes the timestamp back so the
  // permalink computation here will start returning a real URL on the
  // next render via the GET_ACTION refetch.
  const [postActionSlackMessage, postSlackState] = useMutation(POST_ACTION_SLACK_MESSAGE, {
    refetchQueries: [{ query: GET_ACTION, variables: { workspaceId, id: actionId } }],
  })
  const handlePostToSlack = () => {
    void postActionSlackMessage({ variables: { workspaceId, id: actionId } }).catch(() => {
      // Apollo surfaces the error through `postSlackState.error`, which
      // the inline error message below renders. Swallow the rejection
      // here so it does not bubble up as an unhandled promise warning.
    })
  }

  const actionStatuses = useActionStatuses(workspaceId)
  const statusLabel = (id: string) => actionStatuses.label(id)
  const statusColor = (id: string) => {
    const def = actionStatuses.get(id)
    return (actionStatusColorStyle(def?.color).background as string) ?? 'var(--text-muted)'
  }
  // A deep link names one comment, so the feed opens on the Comments tab.
  const [tab, setTab] = useState<Tab>(highlightCommentId ? 'comments' : 'all')

  const commentLimit = highlightCommentId ? DEEP_LINK_COMMENT_LIMIT : pageSize

  const messagesQuery = useQuery<MessagesData>(GET_ACTION_MESSAGES, {
    variables: { workspaceId, id: actionId, limit: pageSize, cursor: null },
    fetchPolicy: 'cache-and-network',
  })
  const eventsQuery = useQuery<EventsData>(GET_ACTION_EVENTS, {
    variables: { workspaceId, id: actionId, limit: pageSize, cursor: null },
    fetchPolicy: 'cache-and-network',
  })
  const commentsQuery = useQuery<CommentsData>(GET_ACTION_COMMENTS, {
    variables: { workspaceId, id: actionId, limit: commentLimit, cursor: null },
    fetchPolicy: 'cache-and-network',
  })

  const refetchComments = [
    { query: GET_ACTION_COMMENTS, variables: { workspaceId, id: actionId, limit: commentLimit, cursor: null } },
  ]
  const [updateComment, updateState] = useMutation(UPDATE_ACTION_COMMENT, { refetchQueries: refetchComments })
  const [deleteComment, deleteState] = useMutation(DELETE_ACTION_COMMENT, { refetchQueries: refetchComments })

  const [editingId, setEditingId] = useState<string | null>(null)
  const [editingBody, setEditingBody] = useState('')
  const [confirmDeleteId, setConfirmDeleteId] = useState<string | null>(null)

  const handleStartEdit = (comment: ActionComment) => {
    setEditingId(comment.id)
    setEditingBody(comment.body)
  }
  const handleCancelEdit = () => {
    setEditingId(null)
    setEditingBody('')
  }
  const handleSaveEdit = (comment: ActionComment) => {
    if (editingBody.trim() === '' || editingBody === comment.body) {
      handleCancelEdit()
      return
    }
    void updateComment({
      variables: { workspaceId, input: { actionId, commentId: comment.id, body: editingBody } },
    })
      .then(() => handleCancelEdit())
      .catch(() => {
        // Apollo surfaces the failure through updateState.error, rendered
        // inline below. The edit stays open so the text is not lost.
      })
  }
  const handleConfirmDelete = () => {
    const id = confirmDeleteId
    if (!id) return
    void deleteComment({ variables: { workspaceId, input: { actionId, commentId: id } } })
      .then(() => setConfirmDeleteId(null))
      .catch(() => {
        // deleteState.error renders inside the dialog; keep it open so the
        // user sees why nothing happened.
      })
  }
  const usersQuery = useQuery<{ slackUsers: SlackUser[] }>(GET_SLACK_USERS, {
    fetchPolicy: 'cache-first',
  })

  const userIndex: UserIndex = useMemo(() => {
    const byName = new Map<string, string>()
    const byImage = new Map<string, string>()
    const byInitial = new Map<string, string>()
    for (const u of usersQuery.data?.slackUsers ?? []) {
      const display = displayName(u) || u.id
      byName.set(u.id, display)
      byInitial.set(u.id, initialOf(display))
      if (u.imageUrl) byImage.set(u.id, u.imageUrl)
    }
    return { byName, byImage, byInitial }
  }, [usersQuery.data])

  const messages = messagesQuery.data?.action?.messages.items ?? []
  const events = eventsQuery.data?.action?.events.items ?? []
  const comments = commentsQuery.data?.action?.comments.items ?? []
  const messagesCursor = messagesQuery.data?.action?.messages.nextCursor ?? ''
  const eventsCursor = eventsQuery.data?.action?.events.nextCursor ?? ''
  const commentsCursor = commentsQuery.data?.action?.comments.nextCursor ?? ''

  const messageCount = messages.length
  const eventCount = events.length
  const commentCount = comments.length
  const totalCount = messageCount + eventCount + commentCount

  const visible = useMemo(() => {
    type Item =
      | { kind: 'message'; createdAt: string; data: SlackMessage }
      | { kind: 'event'; createdAt: string; data: ActionEvent }
      | { kind: 'comment'; createdAt: string; data: ActionComment }
    const items: Item[] = []
    if (tab !== 'history') {
      for (const m of messages) items.push({ kind: 'message', createdAt: m.createdAt, data: m })
      for (const c of comments) items.push({ kind: 'comment', createdAt: c.createdAt, data: c })
    }
    if (tab !== 'comments') {
      for (const e of events) items.push({ kind: 'event', createdAt: e.createdAt, data: e })
    }
    // Newest first.
    items.sort((a, b) => (a.createdAt < b.createdAt ? 1 : a.createdAt > b.createdAt ? -1 : 0))
    return items
  }, [messages, events, comments, tab])

  const loadingInitial =
    (messagesQuery.loading && messages.length === 0) ||
    (eventsQuery.loading && events.length === 0) ||
    (commentsQuery.loading && comments.length === 0)

  return (
    <div style={styles.root} data-testid="action-activity">
      <div style={styles.header}>
        <div style={styles.title}>
          <span>{t('sectionActivity')}</span>
          <span style={styles.count}>{t('activityCount', { count: totalCount })}</span>
        </div>
        <div style={styles.tabs} role="tablist" aria-label="activity tabs">
          <TabButton label={t('activityTabAll')} active={tab === 'all'} onClick={() => setTab('all')} testId="activity-tab-all" />
          <TabButton label={t('activityTabComments')} active={tab === 'comments'} onClick={() => setTab('comments')} testId="activity-tab-comments" />
          <TabButton label={t('activityTabHistory')} active={tab === 'history'} onClick={() => setTab('history')} testId="activity-tab-history" />
        </div>
      </div>

      {loadingInitial && visible.length === 0 ? (
        <div style={styles.empty}>{t('loading')}</div>
      ) : visible.length === 0 ? (
        <div style={styles.empty}>
          {tab === 'comments' ? t('activityEmptyComments') : tab === 'history' ? t('activityEmptyHistory') : t('activityEmptyAll')}
        </div>
      ) : (
        <div style={styles.feed}>
          <div style={styles.rail} aria-hidden />
          {visible.map((it) => {
            if (it.kind === 'message') {
              return <MessageRow key={`m-${it.data.id}`} message={it.data} userIndex={userIndex} t={t} />
            }
            if (it.kind === 'comment') {
              return (
                <CommentRow
                  key={`c-${it.data.id}`}
                  comment={it.data}
                  userIndex={userIndex}
                  t={t}
                  isOwn={!!user && it.data.authorID === user.sub}
                  highlighted={it.data.id === highlightCommentId}
                  isEditing={editingId === it.data.id}
                  editingBody={editingBody}
                  saving={updateState.loading}
                  editError={editingId === it.data.id && !!updateState.error}
                  onEditingBodyChange={setEditingBody}
                  onStartEdit={() => handleStartEdit(it.data)}
                  onCancelEdit={handleCancelEdit}
                  onSaveEdit={() => handleSaveEdit(it.data)}
                  onRequestDelete={() => setConfirmDeleteId(it.data.id)}
                />
              )
            }
            return <EventRow key={`e-${it.data.id}`} event={it.data} userIndex={userIndex} t={t} statusLabel={statusLabel} statusColor={statusColor} />
          })}
        </div>
      )}

      {(tab !== 'history' && (messagesCursor || commentsCursor)) || (tab !== 'comments' && eventsCursor) ? (
        <div style={styles.loadMoreBar}>
          {tab !== 'history' && commentsCursor && (
            <Button
              variant="ghost"
              onClick={() => {
                void commentsQuery.fetchMore({
                  variables: { workspaceId, id: actionId, limit: commentLimit, cursor: commentsCursor },
                  updateQuery: (prev, { fetchMoreResult }) => {
                    if (!fetchMoreResult?.action || !prev.action) return prev
                    return {
                      action: {
                        ...prev.action,
                        comments: {
                          items: [...prev.action.comments.items, ...fetchMoreResult.action.comments.items],
                          nextCursor: fetchMoreResult.action.comments.nextCursor,
                        },
                      },
                    }
                  },
                })
              }}
              data-testid="activity-load-more-comments"
            >
              {t('messagesLoadMore')}
            </Button>
          )}
          {tab !== 'history' && messagesCursor && (
            <Button
              variant="ghost"
              onClick={() => {
                void messagesQuery.fetchMore({
                  variables: { workspaceId, id: actionId, limit: pageSize, cursor: messagesCursor },
                  updateQuery: (prev, { fetchMoreResult }) => {
                    if (!fetchMoreResult?.action || !prev.action) return prev
                    return {
                      action: {
                        ...prev.action,
                        messages: {
                          items: [...prev.action.messages.items, ...fetchMoreResult.action.messages.items],
                          nextCursor: fetchMoreResult.action.messages.nextCursor,
                        },
                      },
                    }
                  },
                })
              }}
              data-testid="activity-load-more-messages"
            >
              {t('messagesLoadMore')}
            </Button>
          )}
          {tab !== 'comments' && eventsCursor && (
            <Button
              variant="ghost"
              onClick={() => {
                void eventsQuery.fetchMore({
                  variables: { workspaceId, id: actionId, limit: pageSize, cursor: eventsCursor },
                  updateQuery: (prev, { fetchMoreResult }) => {
                    if (!fetchMoreResult?.action || !prev.action) return prev
                    return {
                      action: {
                        ...prev.action,
                        events: {
                          items: [...prev.action.events.items, ...fetchMoreResult.action.events.items],
                          nextCursor: fetchMoreResult.action.events.nextCursor,
                        },
                      },
                    }
                  },
                })
              }}
              data-testid="activity-load-older-events"
            >
              {t('activityLoadOlder')}
            </Button>
          )}
        </div>
      ) : null}

      <div style={styles.composer}>
        <ActionCommentComposer workspaceId={workspaceId} actionId={actionId} pageSize={commentLimit} />
      </div>

      {confirmDeleteId && (
        <Modal
          open
          onClose={() => setConfirmDeleteId(null)}
          title={t('titleDeleteComment')}
          width={420}
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirmDeleteId(null)}>{t('btnCancel')}</Button>
              <Button
                variant="danger"
                onClick={handleConfirmDelete}
                disabled={deleteState.loading}
                data-testid="action-comment-delete-confirm"
              >
                {t('btnDelete')}
              </Button>
            </>
          }
        >
          <p style={{ fontSize: 13, lineHeight: 1.6, margin: 0 }}>{t('msgDeleteCommentConfirm')}</p>
          {deleteState.error && (
            <p style={styles.commentError} role="alert" data-testid="action-comment-delete-error">
              {t('errCommentDeleteFailed')}
            </p>
          )}
        </Modal>
      )}

      {slackPermalink ? (
        <div style={styles.slackLinkRow}>
          <a
            href={slackPermalink}
            target="_blank"
            rel="noreferrer noopener"
            style={styles.slackLink}
            data-testid="activity-slack-link"
          >
            <span aria-hidden>💬</span>
            <span>{t('activityOpenInSlack')}</span>
          </a>
        </div>
      ) : (
        <div style={styles.slackLinkRow}>
          <button
            type="button"
            onClick={handlePostToSlack}
            disabled={postSlackState.loading}
            style={styles.slackPostButton}
            data-testid="activity-slack-post"
          >
            <span aria-hidden>💬</span>
            <span>
              {postSlackState.loading
                ? t('activityPostToSlackPosting')
                : t('activityPostToSlack')}
            </span>
          </button>
          {postSlackState.error && (
            <span style={styles.slackPostError} role="alert" data-testid="activity-slack-post-error">
              {t('activityPostToSlackError')}
            </span>
          )}
        </div>
      )}
    </div>
  )
}

function TabButton({ label, active, onClick, testId }: { label: string; active: boolean; onClick: () => void; testId: string }) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={active}
      onClick={onClick}
      data-testid={testId}
      style={{ ...styles.tab, ...(active ? styles.tabActive : {}) }}
    >
      {label}
    </button>
  )
}

function Avatar({ name, image }: { name: string; image: string | undefined }) {
  return (
    <div style={styles.avatar} aria-hidden>
      {image ? <img src={image} alt="" style={styles.avatarImg} /> : <span>{name}</span>}
    </div>
  )
}

function MessageRow({ message, userIndex, t }: { message: SlackMessage; userIndex: UserIndex; t: (k: MsgKey, p?: Record<string, string | number>) => string }) {
  const resolved = userIndex.byName.get(message.userID)
  const displayName = resolved ?? (message.userName && message.userName !== '' ? message.userName : 'App')
  const avatar = userIndex.byImage.get(message.userID)
  const initial = userIndex.byInitial.get(message.userID) ?? initialOf(displayName)
  const body = renderText(message.text, userIndex.byName)
  return (
    <div style={styles.row}>
      <Avatar name={initial} image={avatar} />
      <div style={styles.body}>
        <div style={styles.messageHead}>
          <span style={styles.messageName}>{displayName}</span>
          <span style={styles.timestamp} title={formatTimestampFull(message.createdAt)}>
            {formatTimestamp(message.createdAt, t('activityToday'))}
          </span>
        </div>
        <div style={styles.messageCard}>{body}</div>
      </div>
    </div>
  )
}

function CommentRow({
  comment,
  userIndex,
  t,
  isOwn,
  highlighted,
  isEditing,
  editingBody,
  saving,
  editError,
  onEditingBodyChange,
  onStartEdit,
  onCancelEdit,
  onSaveEdit,
  onRequestDelete,
}: {
  comment: ActionComment
  userIndex: UserIndex
  t: (k: MsgKey, p?: Record<string, string | number>) => string
  isOwn: boolean
  highlighted: boolean
  isEditing: boolean
  editingBody: string
  saving: boolean
  editError: boolean
  onEditingBodyChange: (value: string) => void
  onStartEdit: () => void
  onCancelEdit: () => void
  onSaveEdit: () => void
  onRequestDelete: () => void
}) {
  const rowRef = useRef<HTMLDivElement | null>(null)
  const name = displayName(comment.author) || userIndex.byName.get(comment.authorID) || comment.authorID
  const avatar = comment.author?.imageUrl || userIndex.byImage.get(comment.authorID)
  const initial = userIndex.byInitial.get(comment.authorID) ?? initialOf(name)

  // Bring a deep-linked comment into view once its row exists. The highlight
  // itself is left in place so the reader can still tell which row the link
  // pointed at after scrolling. scrollIntoView is feature-detected because it
  // is absent in jsdom and in older embedded webviews, where an unguarded call
  // would throw out of the effect and blank the whole feed.
  useEffect(() => {
    if (!highlighted) return
    const row = rowRef.current
    if (row && typeof row.scrollIntoView === 'function') {
      row.scrollIntoView({ block: 'center' })
    }
  }, [highlighted])

  return (
    <div style={styles.row} ref={rowRef} data-testid={`activity-comment-${comment.id}`}>
      <Avatar name={initial} image={avatar} />
      <div style={styles.body}>
        <div style={styles.messageHead}>
          <span style={styles.messageName}>{name}</span>
          <span style={styles.timestamp} title={formatTimestampFull(comment.createdAt)}>
            {formatTimestamp(comment.createdAt, t('activityToday'))}
          </span>
          {comment.edited && (
            <span style={styles.commentEdited} title={formatTimestampFull(comment.updatedAt)}>
              {t('activityCommentEdited')}
            </span>
          )}
          {isOwn && !isEditing && (
            <span style={styles.commentActions}>
              <button
                type="button"
                style={styles.commentActionButton}
                onClick={onStartEdit}
                aria-label={t('ariaEditComment')}
                data-testid={`action-comment-edit-${comment.id}`}
              >
                {t('btnEdit')}
              </button>
              <button
                type="button"
                style={styles.commentActionButton}
                onClick={onRequestDelete}
                aria-label={t('ariaDeleteComment')}
                data-testid={`action-comment-delete-${comment.id}`}
              >
                {t('btnDelete')}
              </button>
            </span>
          )}
        </div>
        {isEditing ? (
          <div>
            <MarkdownEditor
              value={editingBody}
              onChange={onEditingBodyChange}
              disabled={saving}
              testId={`action-comment-editor-${comment.id}`}
            />
            <div style={styles.commentEditFooter}>
              <Button variant="primary" size="sm" onClick={onSaveEdit} disabled={saving} data-testid={`action-comment-save-${comment.id}`}>
                {saving ? t('btnSaving') : t('btnSave')}
              </Button>
              <Button variant="ghost" size="sm" onClick={onCancelEdit} disabled={saving}>
                {t('btnCancel')}
              </Button>
              {editError && (
                <span style={styles.commentError} role="alert" data-testid={`action-comment-update-error-${comment.id}`}>
                  {t('errCommentUpdateFailed')}
                </span>
              )}
            </div>
          </div>
        ) : (
          <div style={highlighted ? { ...styles.commentCard, ...styles.commentCardHighlighted } : styles.commentCard}>
            <MarkdownContent source={comment.body} />
          </div>
        )}
      </div>
    </div>
  )
}

function EventRow({ event, userIndex, t, statusLabel, statusColor }: {
  event: ActionEvent
  userIndex: UserIndex
  t: (k: MsgKey, p?: Record<string, string | number>) => string
  statusLabel: (id: string) => string
  statusColor: (id: string) => string
}) {
  const actorName = event.actorID
    ? (displayName(event.actor) || userIndex.byName.get(event.actorID) || event.actorID)
    : t('activityActorSystem')

  return (
    <div style={styles.row} data-testid={`activity-event-${event.kind.toLowerCase()}`}>
      <div style={styles.eventIcon} aria-hidden>{EVENT_ICON[event.kind]}</div>
      <div style={styles.eventLine}>
        <span style={styles.eventActor}>{actorName}</span>
        <span style={styles.eventVerb}>{t(EVENT_KEY_MAP[event.kind])}</span>
        <EventDelta event={event} userIndex={userIndex} t={t} statusLabel={statusLabel} statusColor={statusColor} />
        <span style={styles.eventTime} title={formatTimestampFull(event.createdAt)}>
          {event.kind === 'CREATED'
            ? formatTimestamp(event.createdAt, t('activityToday'))
            : formatTimestampShort(event.createdAt)}
        </span>
      </div>
    </div>
  )
}

function EventDelta({ event, userIndex, t, statusLabel, statusColor }: {
  event: ActionEvent
  userIndex: UserIndex
  t: (k: MsgKey, p?: Record<string, string | number>) => string
  statusLabel: (id: string) => string
  statusColor: (id: string) => string
}) {
  switch (event.kind) {
    case 'CREATED':
      return null
    case 'TITLE_CHANGED':
      return (
        <span style={styles.inline}>
          {event.oldValue && <span style={styles.titleChip} title={event.oldValue}>{event.oldValue}</span>}
          <span style={styles.arrow} aria-hidden>{t('activityArrowTo')}</span>
          <span style={{ ...styles.titleChip, ...styles.titleChipNew }} title={event.newValue}>{event.newValue}</span>
        </span>
      )
    case 'STATUS_CHANGED':
      return (
        <span style={styles.inline}>
          {event.oldValue && <StatusPill status={event.oldValue} statusLabel={statusLabel} statusColor={statusColor} />}
          <span style={styles.arrow} aria-hidden>{t('activityArrowTo')}</span>
          {event.newValue && <StatusPill status={event.newValue} statusLabel={statusLabel} statusColor={statusColor} />}
        </span>
      )
    case 'ASSIGNEE_CHANGED':
      return (
        <span style={styles.inline}>
          {event.oldValue
            ? <UserPill userID={event.oldValue} userIndex={userIndex} />
            : <span style={styles.titleChip}>{t('activityCleared')}</span>}
          <span style={styles.arrow} aria-hidden>{t('activityArrowTo')}</span>
          {event.newValue
            ? <UserPill userID={event.newValue} userIndex={userIndex} />
            : <span style={styles.titleChip}>{t('activityCleared')}</span>}
        </span>
      )
  }
}

function StatusPill({ status, statusLabel, statusColor }: { status: string; statusLabel: (id: string) => string; statusColor: (id: string) => string }) {
  return (
    <span style={styles.statusPill}>
      <span style={{ ...styles.statusDot, background: statusColor(status) }} />
      {statusLabel(status)}
    </span>
  )
}

function UserPill({ userID, userIndex }: { userID: string; userIndex: UserIndex }) {
  const name = userIndex.byName.get(userID) ?? userID
  const image = userIndex.byImage.get(userID)
  const initial = userIndex.byInitial.get(userID) ?? initialOf(name)
  return (
    <span style={styles.userPill}>
      {image ? <img src={image} alt="" style={styles.userPillImg} /> : <span style={styles.userPillFallback}>{initial}</span>}
      <span>{name}</span>
    </span>
  )
}
