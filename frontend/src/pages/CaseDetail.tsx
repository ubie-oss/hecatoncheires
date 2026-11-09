import { useState, useMemo } from 'react'
import { useNavigate, useParams, Link, useLocation } from 'react-router'
import { useQuery, useMutation } from '@apollo/client'
import {
  GET_CASE,
  GET_CASE_MEMBERS,
  CLOSE_CASE,
  REOPEN_CASE,
  DELETE_CASE,
  UPDATE_CASE,
  ASSIGN_CASE,
  UNASSIGN_CASE,
  SYNC_CASE_CHANNEL_USERS,
  ARCHIVE_CASE,
  UNARCHIVE_CASE,
  GET_CASES,
} from '../graphql/case'
import { diffAssignees } from '../utils/assignees'
import { UPDATE_CASE_STATUS } from '../graphql/caseStatus'
import { DISCARD_DRAFT, GET_DRAFTS } from '../graphql/drafts'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'
import { GET_CASE_LATEST_JOB_RUN } from '../graphql/caseAgent'
import { GET_MEMO_CONFIGURATION } from '../graphql/memo'
import MemoTab from '../components/memo/MemoTab'
import CustomFieldHelpRow from '../components/fields/CustomFieldHelpRow'
import CaseMarkdownFieldSection from '../components/fields/CaseMarkdownFieldSection'
import InlineText from '../components/inline/InlineText'
import InlineLongText from '../components/inline/InlineLongText'
import InlineUserSelect from '../components/inline/InlineUserSelect'
import FilterDropdown from '../components/FilterDropdown'
import { useWorkspace } from '../contexts/workspace-context'
import { useTranslation } from '../i18n'
import { useActionStatuses } from '../hooks/useActionStatuses'
import { useCaseStatuses } from '../hooks/useCaseStatuses'
import { useAssigneeCandidates } from '../hooks/useAssigneeCandidates'
import { actionStatusColorStyle } from '../utils/actionStatusStyle'
import { buildSlackCaseLink } from '../utils/slackLink'
import { displayName } from '../utils/user'
import { CASE_LIST_PAGE_PARAM, CASE_LIST_STATUS_PARAM } from './CaseList'
import Button from '../components/Button'
import Modal from '../components/Modal'
import {
  IconChevLeft,
  IconChevRight,
  IconLock,
  IconCheck,
  IconRefresh,
  IconSearch,
  IconPlus,
  IconCalendar,
  IconSlack,
  IconExt,
  IconDots,
  IconRobot,
  IconX,
  IconFlask,
} from '../components/Icons'
import { Avatar, PrivateBadge, TestBadge, StatusBadge } from '../components/Primitives'
import CaseDeleteDialog from './CaseDeleteDialog'
import ActionForm from './ActionForm'
import ActionModal from './ActionModal'
import CaseForm from './CaseForm'
import styles from './CaseDetail.module.css'

interface User {
  id: string
  name: string
  realName: string
  imageUrl?: string | null
}

function formatTimestamp(iso?: string | null) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString()
}

function formatHHMM(iso?: string | null) {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const hh = String(d.getHours()).padStart(2, '0')
  const mm = String(d.getMinutes()).padStart(2, '0')
  return `${hh}:${mm}`
}

// AgentTileStageIcon mirrors the design's small leading indicator on
// the "Last run · …" line: green check / red X / pulsing blue dot for
// SUCCESS / FAILED / RUNNING, and a neutral dash placeholder when the
// agent has never run for this case.
function AgentTileStageIcon({ stage }: { stage: string | null }) {
  if (stage === 'SUCCESS') {
    return (
      <span className="agent-tile-stage-icon success" aria-hidden>
        <IconCheck size={9} sw={3} />
      </span>
    )
  }
  if (stage === 'FAILED') {
    return (
      <span className="agent-tile-stage-icon failed" aria-hidden>
        <IconX size={9} sw={3} />
      </span>
    )
  }
  if (stage === 'RUNNING') {
    return <span className="agent-tile-stage-icon running" aria-hidden />
  }
  return (
    <span className="agent-tile-stage-icon never" aria-hidden>
      —
    </span>
  )
}

function agentTileStageClass(stage: string | null): string {
  switch (stage) {
    case 'SUCCESS': return 'agent-tile-stage-success'
    case 'FAILED':  return 'agent-tile-stage-failed'
    case 'RUNNING': return 'agent-tile-stage-running'
    default:        return 'agent-tile-stage-never'
  }
}

function agentTileStageLabel(stage: string | null, t: (k: 'caseAgentRunStageSuccess' | 'caseAgentRunStageFailed' | 'caseAgentRunStageRunning' | 'caseDetailAgentNeverRun') => string): string {
  switch (stage) {
    case 'SUCCESS': return t('caseAgentRunStageSuccess')
    case 'FAILED':  return t('caseAgentRunStageFailed')
    case 'RUNNING': return t('caseAgentRunStageRunning')
    default:        return t('caseDetailAgentNeverRun')
  }
}

// formatAgentTileRelative renders an "Xh ago" style string in the
// active locale. The threshold bands match the design mock: <1m
// shows seconds, <1h minutes, <24h hours, then days; past a week we
// fall back to the absolute local date so we never claim "300d ago".
function formatAgentTileRelative(
  iso: string,
  _lang: 'en' | 'ja',
  t: (k: 'caseDetailAgentRelSeconds' | 'caseDetailAgentRelMinutes' | 'caseDetailAgentRelHours' | 'caseDetailAgentRelDays', params: { count: number }) => string,
): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const diffMs = Math.max(0, Date.now() - d.getTime())
  const sec = Math.floor(diffMs / 1000)
  if (sec < 60) return t('caseDetailAgentRelSeconds', { count: sec })
  const min = Math.floor(sec / 60)
  if (min < 60) return t('caseDetailAgentRelMinutes', { count: min })
  const hr = Math.floor(min / 60)
  if (hr < 24) return t('caseDetailAgentRelHours', { count: hr })
  const day = Math.floor(hr / 24)
  if (day < 7) return t('caseDetailAgentRelDays', { count: day })
  return d.toLocaleDateString()
}

export default function CaseDetail() {
  const { id, actionId: actionIdParam } = useParams<{ id: string; actionId?: string }>()
  const caseId = Number(id)
  const openActionId = actionIdParam ? Number(actionIdParam) : null
  const navigate = useNavigate()
  const location = useLocation()
  const { currentWorkspace } = useWorkspace()
  const { t, lang } = useTranslation()
  const actionStatuses = useActionStatuses(currentWorkspace?.id)
  // Thread-mode workspaces attach the configurable status set to the Case.
  const caseStatuses = useCaseStatuses(currentWorkspace?.id)

  // Honour the originating case-list tab and page when going back. CaseList
  // sets location.state.fromStatus / fromPage when the user clicks into a
  // row; if the detail page was opened by direct URL (e.g. Slack deep link),
  // the state is absent and we fall back to the default Open tab, page 1.
  const fromState = location.state as { fromStatus?: string; fromPage?: number; fromFieldFilters?: string } | null
  const ALLOWED_FROM_STATUS = ['closed', 'draft', 'all', 'archived'] as const
  const safeFromStatus = ALLOWED_FROM_STATUS.find((s) => s === fromState?.fromStatus)
  // Page 1 is the implicit default and is never emitted, matching CaseList.
  const fromPage = Number(fromState?.fromPage)
  const safeFromPage = Number.isInteger(fromPage) && fromPage > 1 ? fromPage : null
  const caseListUrl = useMemo(() => {
    if (!currentWorkspace) return '/'
    const params = new URLSearchParams()
    if (typeof fromState?.fromFieldFilters === 'string') {
      for (const [key, value] of new URLSearchParams(fromState.fromFieldFilters)) {
        if (key.startsWith('field.')) params.append(key, value)
      }
    }
    if (safeFromStatus) params.set(CASE_LIST_STATUS_PARAM, safeFromStatus)
    if (safeFromPage) params.set(CASE_LIST_PAGE_PARAM, String(safeFromPage))
    const query = params.toString()
    return `/ws/${currentWorkspace.id}/cases${query ? `?${query}` : ''}`
  }, [currentWorkspace, safeFromStatus, safeFromPage, fromState?.fromFieldFilters])

  const [addingAction, setAddingAction] = useState(false)
  const [actionStatusFilters, setActionStatusFilters] = useState<string[]>([])
  const [actionAssigneeFilters, setActionAssigneeFilters] = useState<string[]>([])
  // Two-value toggle: 'open' shows non-archived actions (default); 'archived'
  // shows only archived. Intentionally avoiding a third "All" view because
  // the i18n labels for "Active" and "Archived" sit too close visually —
  // the toggle reads cleanly as a binary choice.
  const [actionView, setActionView] = useState<'open' | 'archived'>('open')
  const [confirmClose, setConfirmClose] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [menuOpen, setMenuOpen] = useState(false)
  const [memberFilter, setMemberFilter] = useState('')
  const [draftEditOpen, setDraftEditOpen] = useState(false)

  const { data, loading, error } = useQuery(GET_CASE, {
    variables: {
      workspaceId: currentWorkspace?.id,
      id: caseId,
      actionsFilter: actionView === 'archived' ? 'ARCHIVED' : 'ACTIVE',
    },
    skip: !currentWorkspace || Number.isNaN(caseId),
  })
  const { data: configData } = useQuery(GET_FIELD_CONFIGURATION, {
    variables: { workspaceId: currentWorkspace?.id },
    skip: !currentWorkspace,
  })

  const c = data?.case
  const isPrivate = !!c?.isPrivate
  const isTest = !!c?.isTest
  const slackChannelID: string = c?.slackChannelID || ''
  const slackChannelURL: string | null = c?.slackChannelURL || null
  const channelUserCount: number = c?.channelUserCount || 0
  // Thread-mode is a WORKSPACE property: in a thread-mode workspace every case
  // uses the thread model (a configurable board status, no lifecycle close
  // button, no Action board of its own) regardless of the individual case's binding flag.
  // We key the UI off the workspace mode, not the per-case isThreadBound, so a
  // case still renders thread-style even if its thread binding is missing.
  const threadMode = caseStatuses.isThreadMode
  // There is no derived `archived` boolean on the wire; a case is archived
  // when archivedAt is non-null.
  const isArchived = !!c?.archivedAt
  const isThreadBound = !!c?.isThreadBound
  const slackThreadTS: string = c?.slackThreadTS || ''
  const slackLink = buildSlackCaseLink(slackChannelURL, slackChannelID, slackThreadTS)
  // Effective board status: fall back to the configured initial status when the
  // case has none yet, so the selector always has a valid value to show.
  const boardStatus: string = c?.boardStatus || caseStatuses.initialId || ''

  const { data: membersData } = useQuery(GET_CASE_MEMBERS, {
    variables: {
      workspaceId: currentWorkspace?.id,
      id: caseId,
      limit: 200,
      offset: 0,
      filter: memberFilter || undefined,
    },
    skip: !currentWorkspace || Number.isNaN(caseId) || !isPrivate || !slackChannelID,
  })

  const { data: latestRunData } = useQuery(GET_CASE_LATEST_JOB_RUN, {
    variables: { workspaceId: currentWorkspace?.id, caseId },
    skip: !currentWorkspace || Number.isNaN(caseId) || c?.status === 'DRAFT',
    fetchPolicy: 'cache-and-network',
  })
  const latestRun = latestRunData?.caseJobRunLogs?.items?.[0] ?? null

  const { data: memoConfigData } = useQuery(GET_MEMO_CONFIGURATION, {
    variables: { workspaceId: currentWorkspace?.id },
    skip: !currentWorkspace,
  })
  const memoEnabled = (memoConfigData?.memoConfiguration?.fields?.length ?? 0) > 0

  const refetchOptions = useMemo(
    () => [
      {
        query: GET_CASE,
        variables: {
          workspaceId: currentWorkspace?.id,
          id: caseId,
          actionsFilter: actionView === 'archived' ? 'ARCHIVED' : 'ACTIVE',
        },
      },
      { query: GET_CASES, variables: { workspaceId: currentWorkspace?.id, status: 'OPEN' } },
      { query: GET_CASES, variables: { workspaceId: currentWorkspace?.id, status: 'CLOSED' } },
    ],
    [currentWorkspace?.id, caseId, actionView],
  )

  const [closeCase, { loading: closing }] = useMutation(CLOSE_CASE, { refetchQueries: refetchOptions })
  const [reopenCase, { loading: reopening }] = useMutation(REOPEN_CASE, { refetchQueries: refetchOptions })
  const [updateCase, { loading: updating }] = useMutation(UPDATE_CASE, { refetchQueries: refetchOptions })
  // Single-case archive / restore runs synchronously, so unlike the Cases
  // page's bulk path a refetch here does reflect the change.
  const [archiveCase, { loading: archivingCase }] = useMutation(ARCHIVE_CASE, { refetchQueries: refetchOptions })
  const [unarchiveCase, { loading: unarchivingCase }] = useMutation(UNARCHIVE_CASE, { refetchQueries: refetchOptions })
  const [assignCase] = useMutation(ASSIGN_CASE, { refetchQueries: refetchOptions })
  const [unassignCase] = useMutation(UNASSIGN_CASE, { refetchQueries: refetchOptions })
  const [updateCaseStatus] = useMutation(UPDATE_CASE_STATUS, { refetchQueries: refetchOptions })
  const [deleteCase, { loading: deleting }] = useMutation(DELETE_CASE, {
    refetchQueries: [
      { query: GET_CASES, variables: { workspaceId: currentWorkspace?.id, status: 'OPEN' } },
      { query: GET_CASES, variables: { workspaceId: currentWorkspace?.id, status: 'CLOSED' } },
    ],
  })
  const [discardDraft, { loading: discardingDraft }] = useMutation(DISCARD_DRAFT, {
    refetchQueries: [{ query: GET_DRAFTS, variables: { workspaceId: currentWorkspace?.id } }],
    awaitRefetchQueries: true,
  })
  const [draftError, setDraftError] = useState<string | null>(null)
  const { users: slackUsers } = useAssigneeCandidates(currentWorkspace?.id)

  const [syncMembers, { loading: syncing }] = useMutation(SYNC_CASE_CHANNEL_USERS, {
    refetchQueries: [{
      query: GET_CASE_MEMBERS,
      variables: { workspaceId: currentWorkspace?.id, id: caseId, limit: 200, offset: 0, filter: memberFilter || undefined },
    }],
  })

  if (loading) {
    return <div className="h-main-inner muted">{t('loading')}</div>
  }
  if (error || !c) {
    return (
      <div className="h-main-inner">
        <div className="card" style={{ padding: 24, color: 'var(--danger)' }}>
          {t('errorCaseNotFound', { caseLabel: configData?.fieldConfiguration?.labels?.case || 'Case' })}
        </div>
      </div>
    )
  }

  const caseLabel = configData?.fieldConfiguration?.labels?.case || 'Case'
  const fields = configData?.fieldConfiguration?.fields || []
  // MARKDOWN fields often hold long text, so they get their own sections in
  // the main column below the description instead of a cramped sidebar row.
  const markdownFields = fields.filter((f: any) => f.type === 'MARKDOWN')
  const sidebarFields = fields.filter((f: any) => f.type !== 'MARKDOWN')

  const handleClose = async () => {
    await closeCase({ variables: { workspaceId: currentWorkspace!.id, id: caseId } })
    setConfirmClose(false)
  }
  const handleReopen = async () => {
    await reopenCase({ variables: { workspaceId: currentWorkspace!.id, id: caseId } })
  }
  const handleArchive = async () => {
    await archiveCase({ variables: { workspaceId: currentWorkspace!.id, id: caseId } })
  }
  const handleUnarchive = async () => {
    await unarchiveCase({ variables: { workspaceId: currentWorkspace!.id, id: caseId } })
  }
  // Thread-mode cases carry the configurable board status; changing it here
  // moves the case between Kanban columns (and closes it when the target is a
  // closed status, server-side).
  const handleBoardStatusChange = async (next: string) => {
    if (!next || next === boardStatus) return
    await updateCaseStatus({ variables: { workspaceId: currentWorkspace!.id, input: { id: caseId, status: next } } })
  }
  const handleDelete = async () => {
    await deleteCase({ variables: { workspaceId: currentWorkspace!.id, id: caseId } })
    setConfirmDelete(false)
    navigate(caseListUrl)
  }
  const handleSync = async () => {
    await syncMembers({ variables: { workspaceId: currentWorkspace!.id, id: caseId } })
  }
  const handleDiscardDraft = async () => {
    setDraftError(null)
    if (!window.confirm(t('draftDiscardConfirm'))) return
    try {
      await discardDraft({ variables: { workspaceId: currentWorkspace!.id, id: caseId } })
      navigate(caseListUrl)
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err)
      setDraftError(t('draftDiscardErrorGeneric', { message: msg }))
    }
  }
  const handleAssigneesChange = async (ids: string[]) => {
    // Assignees move through the delta assign/unassign mutations, not
    // updateCase — diff the picker's full selection against the current set
    // and apply only the change so a concurrent edit is never clobbered.
    const { toAdd, toRemove } = diffAssignees(c.assigneeIDs ?? [], ids)
    if (toAdd.length > 0) {
      await assignCase({ variables: { workspaceId: currentWorkspace!.id, id: caseId, userIDs: toAdd } })
    }
    if (toRemove.length > 0) {
      await unassignCase({ variables: { workspaceId: currentWorkspace!.id, id: caseId, userIDs: toRemove } })
    }
  }
  const handleTitleChange = async (next: string) => {
    if (next === c.title) return
    await updateCase({
      variables: { workspaceId: currentWorkspace!.id, input: { id: caseId, title: next } },
    })
  }
  const handleDescriptionChange = async (next: string) => {
    if (next === (c.description || '')) return
    await updateCase({
      variables: { workspaceId: currentWorkspace!.id, input: { id: caseId, description: next } },
    })
  }
  const handleTestChange = async (next: boolean) => {
    if (next === isTest) return
    await updateCase({
      variables: { workspaceId: currentWorkspace!.id, input: { id: caseId, isTest: next } },
    })
  }
  const handleFieldChange = async (fieldId: string, value: any) => {
    // Send only the changed field. The backend merges this with existing
    // values, so we don't risk re-validating stale entries (e.g. option IDs
    // from a prior config) that would cause the whole update to be rejected.
    let v = value
    // Promote a "YYYY-MM-DD" date to RFC3339 — the backend's date validator
    // requires a full timestamp string.
    if (typeof v === 'string' && /^\d{4}-\d{2}-\d{2}$/.test(v)) {
      v = `${v}T00:00:00Z`
    }
    // Empty single-value: nothing to persist (the backend has no explicit
    // "clear field" path; preserve current value).
    const isEmptySingle = v == null || v === ''
    if (isEmptySingle) return
    await updateCase({
      variables: {
        workspaceId: currentWorkspace!.id,
        input: { id: caseId, fields: [{ fieldId, value: v }] },
      },
    })
  }

  const members: User[] = membersData?.case?.channelUsers?.items || []
  const memberTotal: number = membersData?.case?.channelUserCount ?? channelUserCount

  const totalActions = c.actions?.length || 0
  const doneActions = (c.actions || []).filter((a: any) => a.status === 'COMPLETED').length
  const progressPct = totalActions > 0 ? Math.round((doneActions / totalActions) * 100) : 0

  return (
    <div className="h-main-inner" style={{ maxWidth: 1180 }}>
      {/* top action row */}
      <div className="row" style={{ marginBottom: 18 }}>
        <Button
          variant="ghost"
          size="sm"
          icon={<IconChevLeft size={13} />}
          onClick={() => navigate(caseListUrl)}
        >
          {t('btnBack')}
        </Button>
        {isArchived && (
          <span data-testid="case-archived-badge" className="badge" style={{ marginLeft: 'var(--spacing-sm)' }}>
            {t('badgeArchived')}
          </span>
        )}
        <span className="spacer" />
        {c.status === 'DRAFT' ? (
          <>
            <Button
              size="sm"
              variant="ghost"
              onClick={handleDiscardDraft}
              disabled={discardingDraft}
              data-testid="discard-draft-button"
            >
              {t('draftDiscardButton')}
            </Button>
            <Button
              size="sm"
              variant="primary"
              onClick={() => setDraftEditOpen(true)}
              disabled={discardingDraft}
              data-testid="edit-draft-button"
            >
              {t('btnEdit')}
            </Button>
          </>
        ) : threadMode ? (
          // Thread mode: closing/reopening is driven by the board status
          // selector (a closed status closes the case), so no lifecycle
          // Close/Reopen button is shown here.
          null
        ) : c.status === 'OPEN' ? (
          <Button
            size="sm"
            variant="danger"
            icon={<IconCheck size={13} />}
            onClick={() => setConfirmClose(true)}
            disabled={closing}
            data-testid="close-case-button"
          >
            {t('btnClose')}
          </Button>
        ) : isArchived ? (
          // An archived case cannot go back to OPEN — the server refuses it,
          // because an archived OPEN case would appear in no listing at all.
          // Offer nothing rather than a control whose click does nothing;
          // Unarchive is in the kebab menu.
          null
        ) : (
          <Button
            size="sm"
            icon={<IconRefresh size={13} />}
            onClick={handleReopen}
            disabled={reopening}
          >
            {t('btnReopen')}
          </Button>
        )}
        <div style={{ position: 'relative' }}>
          <Button
            size="sm"
            variant="ghost"
            icon={<IconDots size={14} />}
            onClick={() => setMenuOpen((v) => !v)}
            aria-label={t('btnMore')}
            data-testid="case-menu-button"
          />
          {menuOpen && (
            <>
              <div
                onClick={() => setMenuOpen(false)}
                style={{ position: 'fixed', inset: 0, zIndex: 100 }}
              />
              <div
                role="menu"
                data-testid="case-menu-popover"
                className={styles.kebabMenu}
              >
                {/* Archiving only applies to a CLOSED case, and restoring only
                    to an archived one, so at most one of these is offered —
                    an item the server would refuse is never shown. */}
                {isArchived ? (
                  <button
                    type="button"
                    onClick={() => { setMenuOpen(false); void handleUnarchive() }}
                    disabled={unarchivingCase}
                    data-testid="case-unarchive-menu-item"
                    className={styles.kebabItem}
                  >
                    {t('btnUnarchive')}
                  </button>
                ) : c.status === 'CLOSED' ? (
                  <button
                    type="button"
                    onClick={() => { setMenuOpen(false); void handleArchive() }}
                    disabled={archivingCase}
                    data-testid="case-archive-menu-item"
                    className={styles.kebabItem}
                  >
                    {t('btnArchive')}
                  </button>
                ) : null}
                <button
                  type="button"
                  onClick={() => { setMenuOpen(false); setConfirmDelete(true) }}
                  data-testid="case-delete-menu-item"
                  className={`${styles.kebabItem} ${styles.kebabDanger}`}
                >
                  {t('btnDelete')}
                </button>
              </div>
            </>
          )}
        </div>
      </div>

      {/* main grid */}
      <div className="h-detail-grid">
        {/* left column */}
        <div>
          {/* title row */}
          <div className="h-detail-h">
            <span className="h-detail-id">#{c.id}</span>
            <h1>
              <InlineText
                value={c.title}
                onSave={handleTitleChange}
                ariaLabel={t('labelTitle')}
                variant="title"
                placeholder={t('placeholderAddTitle')}
                disabled={updating}
                testId="case-title"
              />
            </h1>
            {(isPrivate || isTest) && (
              <div className="h-detail-badges">
                {isTest && (
                  <span data-testid="test-badge"><TestBadge label={t('badgeTest')} /></span>
                )}
                {isPrivate && (
                  <span data-testid="private-badge"><PrivateBadge label={t('badgePrivate')} /></span>
                )}
              </div>
            )}
          </div>

          {/* sub-meta */}
          <div className="h-detail-meta">
            <span>
              <IconCalendar size={11} style={{ marginRight: 4, verticalAlign: '-2px' }} />
              {t('labelCreatedTimestamp')} <span className="mono" data-testid="created-timestamp-value" data-ts={c.createdAt ?? ''}>{formatTimestamp(c.createdAt)}</span>
            </span>
            <span>
              {t('labelUpdatedTimestamp')} <span className="mono" data-testid="updated-timestamp-value" data-ts={c.updatedAt ?? ''}>{formatTimestamp(c.updatedAt)}</span>
            </span>
            {c.reporter && (
              <span>
                {t('labelBy')} <span className="name">{displayName(c.reporter)}</span>
              </span>
            )}
            {/* Inline Test toggle — the WebUI edit surface for an OPEN case's
                test flag (drafts use the CaseForm checkbox instead). */}
            <label className="row" style={{ gap: 4, cursor: 'pointer' }} title={t('hintTestCase', { caseLabelLower: caseLabel.toLowerCase() })}>
              <input
                type="checkbox"
                checked={isTest}
                disabled={updating}
                onChange={(e) => { void handleTestChange(e.target.checked) }}
                data-testid="test-case-toggle"
              />
              <IconFlask size={11} sw={2} style={{ verticalAlign: '-1px' }} />
              {t('badgeTest')}
            </label>
          </div>

          {/* private banner — only relevant once the case is linked to a Slack
              channel; drafts have no channel yet and would render an empty
              msgPrivateBanner. */}
          {isPrivate && slackChannelID && (
            <div className="h-banner warn" data-testid="case-private-banner">
              <IconLock size={13} sw={2} />
              <span className="h-banner-text">
                {t('msgPrivateBanner', {
                  count: String(channelUserCount || 0),
                })}
              </span>
            </div>
          )}

          {draftError && (
            <div
              role="alert"
              className="card"
              style={{ padding: 12, marginBottom: 16, color: 'var(--color-error)' }}
              data-testid="draft-action-error"
            >
              {draftError}
            </div>
          )}

          <section className="h-section h-detail-body">
            <div className="h-section-h">
              <span className="h-section-title">{t('labelDescription')}</span>
            </div>
            <InlineLongText
              value={c.description || ''}
              onSave={handleDescriptionChange}
              ariaLabel={t('labelDescription')}
              placeholder={t('placeholderAddDescription')}
              disabled={updating}
              testId="case-description"
              renderMarkdown
            />
          </section>

          {markdownFields.map((f: any) => (
            <CaseMarkdownFieldSection
              key={f.id}
              field={f}
              value={c.fields?.find((x: any) => x.fieldId === f.id)?.value}
              disabled={updating}
              onSave={(v) => handleFieldChange(f.id, v)}
              testId={`case-field-${f.id}`}
            />
          ))}

          {memoEnabled && (
          <section className="h-section">
            <div className="h-section-h">
              <span className="h-section-title">{t('tabMemos')}</span>
            </div>
            <MemoTab
              caseId={caseId}
              workspaceId={currentWorkspace!.id}
              accessDenied={c.accessDenied}
            />
          </section>
          )}

          {/* Related Actions exist for every activated case, in channel and
              thread mode alike. Drafts have no actions yet, so the section is
              hidden for them. */}
          {c.status !== 'DRAFT' && (
          <section className="h-section">
            <div className="h-section-h">
              <span className="h-section-title">{t('sectionRelatedActions')}</span>
              {totalActions > 0 && actionView === 'open' && (
                <>
                  <span className="h-section-progress" data-testid="related-actions-progress">
                    {t('labelProgress', { done: String(doneActions), total: String(totalActions) })}
                  </span>
                  <div className="h-section-bar">
                    <span data-testid="related-actions-progress-bar" style={{ width: `${progressPct}%` }} />
                  </div>
                </>
              )}
              <span className="spacer" />
              {/* A thread-mode workspace's /actions route is the Case board,
                  not an Action board, so there is no action list to open. */}
              {!threadMode && (
              <Button
                size="sm"
                variant="ghost"
                icon={<IconExt size={11} />}
                onClick={() => navigate(`/ws/${currentWorkspace!.id}/actions/case/${c.id}`)}
                data-testid="case-open-in-actions"
              >
                {t('linkOpenInActionList')}
              </Button>
              )}
              {actionView === 'open' && (
                <Button
                  size="sm"
                  icon={<IconPlus size={12} />}
                  onClick={() => setAddingAction(true)}
                  data-testid="add-action-button"
                >
                  {t('btnAddAction')}
                </Button>
              )}
              <div
                className="seg-toggle"
                role="tablist"
                aria-label="Action view"
                data-testid="action-view-toggle"
              >
                <button
                  type="button"
                  role="tab"
                  aria-selected={actionView === 'open'}
                  className={actionView === 'open' ? 'seg-toggle-btn seg-toggle-btn--active' : 'seg-toggle-btn'}
                  onClick={() => setActionView('open')}
                  data-testid="action-view-open"
                >
                  {t('lblViewOpenActions')}
                </button>
                <button
                  type="button"
                  role="tab"
                  aria-selected={actionView === 'archived'}
                  className={actionView === 'archived' ? 'seg-toggle-btn seg-toggle-btn--active' : 'seg-toggle-btn'}
                  onClick={() => setActionView('archived')}
                  data-testid="action-view-archived"
                >
                  {t('lblViewArchivedActions')}
                </button>
              </div>
            </div>

            {totalActions > 0 && (() => {
              const presentStatuses = new Set<string>(c.actions.map((a: any) => a.status))
              const statusOpts = actionStatuses.statuses
                .filter((s) => presentStatuses.has(s.id))
                .map((s) => ({ value: s.id, label: actionStatuses.label(s.id) }))

              const assigneeMap = new Map<string, { value: string; label: string }>()
              let anyUnassigned = false
              c.actions.forEach((a: any) => {
                if (!a.assignee) {
                  anyUnassigned = true
                  return
                }
                if (!assigneeMap.has(a.assignee.id)) {
                  assigneeMap.set(a.assignee.id, { value: a.assignee.id, label: displayName(a.assignee) })
                }
              })
              const assigneeOpts: Array<{ value: string; label: string }> = []
              if (anyUnassigned) assigneeOpts.push({ value: 'NONE', label: t('filterUnassigned') })
              assigneeOpts.push(...Array.from(assigneeMap.values()))

              const filtered = (c.actions as any[]).filter((a) => {
                if (actionStatusFilters.length > 0 && !actionStatusFilters.includes(a.status)) return false
                if (actionAssigneeFilters.length > 0) {
                  const wantUnassigned = actionAssigneeFilters.includes('NONE')
                  const isUnassigned = !a.assignee
                  const userIds = actionAssigneeFilters.filter((id) => id !== 'NONE')
                  const matchesUser = userIds.length > 0
                    && a.assignee != null && userIds.includes(a.assignee.id)
                  if (!((wantUnassigned && isUnassigned) || matchesUser)) return false
                }
                return true
              })

              return (
                <>
                  <div className="h-filter-bar">
                    <FilterDropdown
                      testId="related-action-status-filter"
                      label="Status"
                      allLabel={t('filterAllShort')}
                      options={statusOpts}
                      value={actionStatusFilters}
                      onChange={setActionStatusFilters}
                    />
                    <FilterDropdown
                      testId="related-action-assignee-filter"
                      label="Assignee"
                      allLabel={t('filterAllAssigneesShort')}
                      options={assigneeOpts}
                      value={actionAssigneeFilters}
                      onChange={setActionAssigneeFilters}
                    />
                    <span className="h-filter-count">{t('labelCount', { count: filtered.length })}</span>
                  </div>

                  {filtered.length === 0 ? (
                    <div className="card" style={{ padding: 16, textAlign: 'center' }} data-testid="related-actions-empty-after-filter">
                      <p className="muted" style={{ fontSize: 12, margin: 0 }}>{t('emptyActionsFilter')}</p>
                    </div>
                  ) : (
                    <div className="h-action-list">
                      {filtered.map((a: any) => {
                        const status = String(a.status)
                        const def = actionStatuses.get(status)
                        const colorStyle = actionStatusColorStyle(def?.color)
                        const closed = actionStatuses.isClosed(status)
                        return (
                          <Link
                            key={a.id}
                            // Opening the Action modal stays on this Case, so the
                            // list's fromStatus / fromPage must ride along —
                            // react-router drops location.state otherwise and the
                            // Back button would return to the default tab, page 1.
                            state={location.state}
                            to={`/ws/${currentWorkspace!.id}/cases/${c.id}/actions/${a.id}`}
                            className="h-action-row"
                            data-testid="case-related-action"
                            data-status={status}
                          >
                            <span className="h-action-pip" style={{ ...colorStyle, borderRadius: '50%' }} />
                            <span className="h-action-title">{a.title}</span>
                            <span
                              className="h-action-status"
                              style={{ color: colorStyle.background as string }}
                              data-testid="case-related-action-status"
                            >
                              {actionStatuses.label(status)}
                            </span>
                            {closed && a.updatedAt && (
                              <span className="h-action-meta" data-testid="case-related-action-completed-at">
                                {t('labelCompleted')}<span className="mono">{formatHHMM(a.updatedAt)}</span>
                              </span>
                            )}
                            {a.assignee && (
                              <Avatar
                                size="sm"
                                name={a.assignee.name}
                                realName={a.assignee.realName}
                                imageUrl={a.assignee.imageUrl}
                              />
                            )}
                          </Link>
                        )
                      })}
                    </div>
                  )}
                </>
              )
            })()}

            {totalActions === 0 && (
              <div className="card" style={{ padding: 24, textAlign: 'center' }}>
                <h3 style={{ fontSize: 14, margin: 0 }}>{t('emptyActionsTitle')}</h3>
                <p className="muted" style={{ fontSize: 12, marginTop: 6 }}>{t('emptyActionsDesc')}</p>
                <div style={{ marginTop: 12 }}>
                  <Button
                    size="sm"
                    icon={<IconPlus size={12} />}
                    onClick={() => setAddingAction(true)}
                    data-testid="empty-add-action-button"
                  >
                    {t('btnAddAction')}
                  </Button>
                </div>
              </div>
            )}
          </section>
          )}

        </div>

        {/* right column / sidebar */}
        <aside className="h-aside">
          {slackLink && (
            <section className="h-aside-section h-aside-section-slack">
              <a
                className="slack-link"
                data-testid="aside-slack-link"
                href={slackLink}
                target="_blank"
                rel="noreferrer noopener"
              >
                <IconSlack size={11} />{isThreadBound ? t('labelSlackThread') : t('labelOpenInSlack')}
                <IconExt size={10} />
              </a>
            </section>
          )}

          <section className="h-aside-section">
            <div className="h-aside-h">
              <span className="h-aside-title">{t('labelStatus')}</span>
            </div>
            <div data-testid="aside-status-display" className="h-status-strong">
              {threadMode ? (
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                  <span
                    className="pip"
                    style={{
                      width: 8,
                      height: 8,
                      borderRadius: '50%',
                      ...actionStatusColorStyle(caseStatuses.get(boardStatus)?.color),
                    }}
                  />
                  <select
                    data-testid="aside-board-status"
                    value={boardStatus}
                    // An archived case takes no board writes: the server
                    // rejects them, so the control is disabled rather than
                    // left to fail silently. Unarchive it first.
                    disabled={isArchived}
                    onChange={(e) => { void handleBoardStatusChange(e.target.value) }}
                    style={{
                      border: '1px solid var(--border-default)',
                      borderRadius: '0.375rem',
                      background: 'var(--bg-paper)',
                      color: 'var(--text-body)',
                      fontFamily: 'inherit',
                      fontSize: 13,
                      padding: '0.125rem 0.375rem',
                    }}
                  >
                    {caseStatuses.statuses.map((s) => (
                      <option key={s.id} value={s.id}>{s.name || s.id}</option>
                    ))}
                  </select>
                </span>
              ) : (
                <StatusBadge
                  status={c.status}
                  labelOpen={t('statusOpen')}
                  labelClosed={t('statusClosed')}
                />
              )}
            </div>
          </section>

          {c.status !== 'DRAFT' && (
            <section className="h-aside-section">
              <div className="h-aside-h">
                <span className="h-aside-title">{t('caseDetailAgentLinkLabel')}</span>
              </div>
              {/* 2-line tile (designer pick "B"): square accent icon +
                  bold title + "Last run · <stage> · <relative>" subline.
                  Doubles as the page entrypoint AND a passive read-out
                  of the most recent Job run's outcome. */}
              <Link
                to={`/ws/${currentWorkspace!.id}/cases/${caseId}/agent`}
                className="agent-tile"
                data-testid="case-agent-link"
              >
                <span className="agent-tile-icon">
                  <IconRobot size={20} sw={1.6} />
                </span>
                <div className="agent-tile-body">
                  <div className="agent-tile-title">
                    {t('caseDetailAgentTileTitle')}
                  </div>
                  <div className="agent-tile-sub">
                    <AgentTileStageIcon stage={latestRun?.stage ?? null} />
                    <span>{t('caseDetailAgentLastRun')}</span>
                    <span className="agent-tile-sub-sep">·</span>
                    <span className={agentTileStageClass(latestRun?.stage ?? null)}>
                      {agentTileStageLabel(latestRun?.stage ?? null, t)}
                    </span>
                    {latestRun?.startedAt && (
                      <>
                        <span className="agent-tile-sub-sep">·</span>
                        <span>{formatAgentTileRelative(latestRun.startedAt, lang, t)}</span>
                      </>
                    )}
                  </div>
                </div>
                <IconChevRight size={14} className="agent-tile-chev" />
              </Link>
            </section>
          )}

          <section className="h-aside-section" data-testid="case-assignees-inline">
            <div className="h-aside-h">
              <span className="h-aside-title">{t('sectionAssignees')}</span>
            </div>
            <InlineUserSelect
              isMulti
              users={slackUsers}
              value={c.assigneeIDs || []}
              onSave={(ids) => handleAssigneesChange(ids as string[])}
              ariaLabel={t('sectionAssignees')}
              placeholder={t('placeholderAddAssignees')}
              disabled={updating}
              testId="case-assignees"
            />
          </section>

          {(c.reporter || c.reporterID) && (
            <section className="h-aside-section" data-testid="case-reporter-section">
              <div className="h-aside-h">
                <span className="h-aside-title">{t('labelReporter')}</span>
              </div>
              {c.reporter ? (
                <div className="row" style={{ gap: 8, alignItems: 'center' }}>
                  <Avatar size="sm" name={c.reporter.name} realName={c.reporter.realName} imageUrl={c.reporter.imageUrl} />
                  <span style={{ fontSize: 13 }}>{displayName(c.reporter)}</span>
                </div>
              ) : (
                <div className="row" style={{ gap: 8, alignItems: 'center' }}>
                  <Avatar size="sm" name={c.reporterID || ''} realName={c.reporterID || ''} />
                  <span className="mono soft" style={{ fontSize: 12 }}>{c.reporterID}</span>
                </div>
              )}
            </section>
          )}

          {sidebarFields.length > 0 && (
            <section className="h-aside-section" data-testid="case-fields-inline">
              <div className="h-aside-h">
                <span className="h-aside-title">{t('sectionFields')}</span>
              </div>
              <div className="kv-list">
                {sidebarFields.map((f: any) => {
                  const fv = c.fields?.find((x: any) => x.fieldId === f.id)
                  return (
                    <CustomFieldHelpRow
                      key={f.id}
                      field={f}
                      value={fv?.value}
                      display={fv?.display}
                      users={slackUsers}
                      disabled={updating}
                      onSave={(v) => handleFieldChange(f.id, v)}
                      testId={`case-field-${f.id}`}
                    />
                  )
                })}
              </div>
            </section>
          )}

          {isPrivate && slackChannelID && (
            <section className="h-aside-section" data-testid="channel-members-section">
              <div className="h-aside-h">
                <span className="h-aside-title">
                  {t('sectionChannelMembers', { count: memberTotal })}
                </span>
                <span className="spacer" />
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={handleSync}
                  disabled={syncing}
                  data-testid="sync-members-button"
                  icon={<IconRefresh size={12} />}
                >
                  {t('btnSync')}
                </Button>
              </div>
              <div className="h-search" style={{ width: '100%', marginLeft: 0, marginBottom: 8 }}>
                <IconSearch size={13} />
                <input
                  value={memberFilter}
                  onChange={(e) => setMemberFilter(e.target.value)}
                  placeholder={t('placeholderFilterMembers')}
                  data-testid="member-search-filter"
                  style={{ flex: 1, border: 'none', background: 'transparent', outline: 'none', fontFamily: 'inherit', fontSize: 12.5, color: 'var(--fg)' }}
                />
              </div>
              <div className="col" style={{ gap: 0 }}>
                {members.map((m) => (
                  <div key={m.id} className={styles.memberItem}>
                    <Avatar size="sm" name={m.name} realName={m.realName} imageUrl={m.imageUrl} />
                    <span className={`name truncate ${styles.name}`}>{displayName(m)}</span>
                  </div>
                ))}
              </div>
            </section>
          )}
        </aside>
      </div>

      {addingAction && (
        <ActionForm
          action={null}
          defaultCaseID={c.id}
          onClose={() => setAddingAction(false)}
        />
      )}

      {openActionId !== null && (
        <ActionModal
          actionId={openActionId}
          // Closing the modal navigates back to this same Case, so carry the
          // list's fromStatus / fromPage forward the way the opening Link does.
          onClose={() => navigate(`/ws/${currentWorkspace!.id}/cases/${c.id}`, { state: location.state })}
        />
      )}

      {confirmClose && (
        <Modal
          open
          onClose={() => setConfirmClose(false)}
          title={t('titleCloseCase', { caseLabel })}
          width={460}
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirmClose(false)}>{t('btnCancel')}</Button>
              <Button variant="primary" onClick={handleClose} disabled={closing} data-testid="confirm-close-button">
                {t('btnClose')}
              </Button>
            </>
          }
        >
          <div
            style={{ fontSize: 13, lineHeight: 1.6 }}
            dangerouslySetInnerHTML={{ __html: t('msgCloseCaseConfirm', { title: escapeHtml(c.title) }) }}
          />
        </Modal>
      )}

      {confirmDelete && (
        <CaseDeleteDialog
          caseTitle={c.title}
          caseLabel={caseLabel}
          deleting={deleting}
          onCancel={() => setConfirmDelete(false)}
          onConfirm={handleDelete}
        />
      )}

      {draftEditOpen && (
        <CaseForm
          caseItem={{
            id: c.id,
            title: c.title,
            description: c.description ?? '',
            isPrivate: !!c.isPrivate,
            isTest: !!c.isTest,
            assigneeIDs: c.assigneeIDs ?? [],
            fields: (c.fields ?? []).map((f: any) => ({ fieldId: f.fieldId, value: f.value })),
            status: 'DRAFT',
          }}
          onClose={() => setDraftEditOpen(false)}
          onSubmitted={() => {
            // After successful Submit the page already auto-refreshes via
            // the refetched GET_CASE; close the modal so the user sees
            // the freshly OPEN case detail.
            setDraftEditOpen(false)
          }}
        />
      )}
    </div>
  )
}

function escapeHtml(s: string) {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
}
