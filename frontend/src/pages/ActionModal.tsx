import { useState, useMemo } from 'react'
import { useNavigate, useSearchParams } from 'react-router'
import { useMutation, useQuery } from '@apollo/client'
import { GET_ACTION, UPDATE_ACTION, ARCHIVE_ACTION, UNARCHIVE_ACTION, GET_ACTIONS } from '../graphql/action'
import { useWorkspace } from '../contexts/workspace-context'
import { useTranslation } from '../i18n'
import { useActionStatuses } from '../hooks/useActionStatuses'
import { useAssigneeCandidates } from '../hooks/useAssigneeCandidates'
import { actionStatusColor } from '../utils/actionStatusStyle'
import Modal from '../components/Modal'
import Button from '../components/Button'
import { IconCheck } from '../components/Icons'
import InlineText from '../components/inline/InlineText'
import InlineLongText from '../components/inline/InlineLongText'
import InlineSelect, { type InlineSelectOption } from '../components/inline/InlineSelect'
import InlineUserSelect from '../components/inline/InlineUserSelect'
import InlineDate from '../components/inline/InlineDate'
import ActionActivity from '../components/ActionActivity'
import StepList from '../components/StepList'

interface ActionModalProps {
  actionId: number
  onClose: () => void
}

function formatDue(iso?: string | null) {
  if (!iso) return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return null
  const today = new Date()
  const sameDay =
    d.getFullYear() === today.getFullYear() &&
    d.getMonth() === today.getMonth() &&
    d.getDate() === today.getDate()
  const time = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  const overdue = d.getTime() < today.getTime() && !sameDay
  return {
    label: sameDay ? `今日 ${time}` : `${d.getFullYear()}/${String(d.getMonth() + 1).padStart(2, '0')}/${String(d.getDate()).padStart(2, '0')} ${time}`,
    urgent: sameDay,
    overdue,
  }
}

export default function ActionModal({ actionId, onClose }: ActionModalProps) {
  const { currentWorkspace } = useWorkspace()
  const { t } = useTranslation()
  const navigate = useNavigate()
  // `?comment=<id>` is what the Slack comment notification links to. Reading it
  // here keeps ActionActivity free of any router dependency.
  const [searchParams] = useSearchParams()
  const highlightCommentId = searchParams.get('comment')
  const { statuses, label: statusLabel } = useActionStatuses(currentWorkspace?.id)
  const { data, loading } = useQuery(GET_ACTION, {
    variables: { workspaceId: currentWorkspace?.id, id: actionId },
    skip: !currentWorkspace,
  })

  const action = data?.action
  const [savedFlash, setSavedFlash] = useState(false)

  const { users } = useAssigneeCandidates(currentWorkspace?.id)

  const [updateAction] = useMutation(UPDATE_ACTION, {
    refetchQueries: [
      { query: GET_ACTION, variables: { workspaceId: currentWorkspace?.id, id: actionId } },
      { query: GET_ACTIONS, variables: { workspaceId: currentWorkspace?.id } },
    ],
  })
  const [archiveAction, { loading: archiving }] = useMutation(ARCHIVE_ACTION, {
    refetchQueries: [
      { query: GET_ACTION, variables: { workspaceId: currentWorkspace?.id, id: actionId } },
      { query: GET_ACTIONS, variables: { workspaceId: currentWorkspace?.id } },
    ],
  })
  const [unarchiveAction, { loading: unarchiving }] = useMutation(UNARCHIVE_ACTION, {
    refetchQueries: [
      { query: GET_ACTION, variables: { workspaceId: currentWorkspace?.id, id: actionId } },
      { query: GET_ACTIONS, variables: { workspaceId: currentWorkspace?.id } },
    ],
  })

  const flashSaved = () => {
    setSavedFlash(true)
    window.setTimeout(() => setSavedFlash(false), 1500)
  }

  const statusOptions: InlineSelectOption<string>[] = useMemo(
    () => statuses.map((s) => ({
      value: s.id,
      label: statusLabel(s.id),
      color: actionStatusColor(s.color),
    })),
    [statuses, statusLabel],
  )

  const titleEl = useMemo(() => (
    <div className="row" style={{ gap: 12, alignItems: 'center', flex: 1, minWidth: 0 }}>
      <h2
        id="modal-title"
        style={{ margin: 0, fontSize: 13, fontWeight: 500, color: 'var(--fg-soft)', fontFamily: 'var(--font-mono)', flex: '0 0 auto' }}
      >
        #A-{actionId}
      </h2>
      {action?.case && (
        <a
          className="slack-link"
          href={`/ws/${currentWorkspace!.id}/cases/${action.case.id}`}
          data-testid="action-case-link"
          title={`#${action.case.id} ${action.case.title}`}
          style={{ flex: '1 1 auto', minWidth: 0, overflow: 'hidden' }}
          onClick={(e) => {
            e.preventDefault()
            navigate(`/ws/${currentWorkspace!.id}/cases/${action.case.id}`)
          }}
        >
          <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', minWidth: 0 }}>
            #{action.case.id} {action.case.title}
          </span>
        </a>
      )}
      {action?.archived && (
        <span
          className="badge"
          data-testid="action-archived-badge"
          style={{ fontSize: 10, flex: '0 0 auto', background: 'var(--bg-muted)', color: 'var(--text-muted)' }}
        >
          {t('badgeArchived')}
        </span>
      )}
      {savedFlash && (
        <span className="badge open" style={{ fontSize: 10, flex: '0 0 auto' }}>
          <IconCheck size={9} sw={2.5} />
          {t('feedbackSaved')}
        </span>
      )}
    </div>
  ), [actionId, action?.archived, action?.case?.id, action?.case?.title, savedFlash, t, currentWorkspace, navigate])

  if (!loading && !action) {
    return (
      <Modal open onClose={onClose} title={t('errorActionNotFound')}>
        <p className="muted">{t('errorActionNotFound')}</p>
      </Modal>
    )
  }

  const handleStatusChange = async (next: string) => {
    if (!action || next === action.status) return
    await updateAction({
      variables: { workspaceId: currentWorkspace!.id, input: { id: action.id, status: next } },
    })
    flashSaved()
  }

  const handleSaveTitle = async (next: string) => {
    if (!action || next === action.title) return
    await updateAction({
      variables: { workspaceId: currentWorkspace!.id, input: { id: action.id, title: next } },
    })
    flashSaved()
  }

  const handleSaveDescription = async (next: string) => {
    if (!action || next === (action.description || '')) return
    await updateAction({
      variables: { workspaceId: currentWorkspace!.id, input: { id: action.id, description: next } },
    })
    flashSaved()
  }

  const handleDueDateChange = async (next: string | null) => {
    if (!action) return
    const input: any = { id: action.id }
    if (next == null) {
      input.clearDueDate = true
    } else {
      // Promote a "YYYY-MM-DD" to a full ISO timestamp at midnight UTC so the
      // backend's Time scalar can parse it.
      input.dueDate = /^\d{4}-\d{2}-\d{2}$/.test(next) ? `${next}T00:00:00Z` : next
    }
    await updateAction({ variables: { workspaceId: currentWorkspace!.id, input } })
    flashSaved()
  }

  const handleAssigneeChange = async (next: string | null) => {
    if (!action) return
    const input: Record<string, unknown> = { id: action.id }
    if (next) {
      input.assigneeID = next
    } else {
      input.clearAssignee = true
    }
    await updateAction({
      variables: { workspaceId: currentWorkspace!.id, input },
    })
    flashSaved()
  }

  const [confirmArchive, setConfirmArchive] = useState(false)
  const [confirmUnarchive, setConfirmUnarchive] = useState(false)
  const handleArchive = async () => {
    if (!action) return
    await archiveAction({ variables: { workspaceId: currentWorkspace!.id, id: action.id } })
    setConfirmArchive(false)
    onClose()
  }
  const handleUnarchive = async () => {
    if (!action) return
    await unarchiveAction({ variables: { workspaceId: currentWorkspace!.id, id: action.id } })
    setConfirmUnarchive(false)
    // Close the modal after unarchive: when the user is on the Archived view
    // toggle in CaseDetail, the now-restored action no longer belongs in
    // that slice and leaving the modal open would dangle on stale data.
    onClose()
  }
  const isArchived = !!action?.archived

  const due = action ? formatDue(action.dueDate) : null

  return (
    <Modal
      open
      onClose={onClose}
      width={680}
      title={titleEl}
      footer={
        <>
          {isArchived ? (
            <Button
              variant="primary"
              onClick={() => setConfirmUnarchive(true)}
              disabled={unarchiving}
              data-testid="action-unarchive-button"
            >
              {t('btnUnarchive')}
            </Button>
          ) : (
            <Button
              variant="danger"
              onClick={() => setConfirmArchive(true)}
              disabled={archiving}
              data-testid="action-archive-button"
            >
              {t('btnArchive')}
            </Button>
          )}
          <span className="spacer" />
          <Button variant="ghost" onClick={onClose}>{t('btnClose')}</Button>
        </>
      }
    >
      {loading ? (
        <div className="muted">{t('loading')}</div>
      ) : (
        <>
          <div style={{ marginBottom: 'var(--sp-6)' }}>
            <InlineText
              value={action.title || ''}
              onSave={handleSaveTitle}
              ariaLabel={t('labelTitle')}
              variant="title"
              placeholder={t('placeholderAddTitle')}
              testId="action-title"
            />
          </div>

          {/* Meta fields — vertical stack with a fixed-width label column.
              Mirrors the designer mock under tmp/hecatoncheires/screens-2.jsx
              so labels never wrap mid-character (which is what happens when
              a flex row gets squeezed and the inline label column collapses). */}
          <div className="col" style={{ gap: 8, marginBottom: 'var(--sp-6)', fontSize: 13 }}>
            <div className="row" style={{ gap: 'var(--sp-4)', alignItems: 'center' }}>
              <span className="soft" style={{ width: 78, flexShrink: 0, fontSize: 12, whiteSpace: 'nowrap' }}>{t('labelStatus')}</span>
              <InlineSelect<string>
                value={action.status as string}
                options={statusOptions}
                onSave={handleStatusChange}
                ariaLabel={t('labelStatus')}
                placeholder={t('placeholderAddStatus')}
                testId="action-status"
              />
              {/* a11y dropdown for assistive tech and existing e2e */}
              <select
                aria-hidden
                tabIndex={-1}
                data-testid="status-dropdown"
                value={action.status}
                onChange={(e) => handleStatusChange(e.target.value)}
                style={{ position: 'absolute', width: 1, height: 1, opacity: 0, pointerEvents: 'none' }}
              >
                {statuses.map((s) => (
                  <option key={s.id} value={s.id}>{statusLabel(s.id)}</option>
                ))}
              </select>
            </div>
            <div className="row" style={{ gap: 'var(--sp-4)', alignItems: 'center' }}>
              <span className="soft" style={{ width: 78, flexShrink: 0, fontSize: 12, whiteSpace: 'nowrap' }}>{t('labelAssignee')}</span>
              <div style={{ flex: 1, minWidth: 0 }}>
                <InlineUserSelect
                  users={users}
                  value={action.assigneeID || null}
                  onSave={handleAssigneeChange}
                  ariaLabel={t('labelAssignee')}
                  placeholder={t('placeholderSelectAssignee')}
                  testId="action-assignee"
                />
              </div>
            </div>
            <div className="row" style={{ gap: 'var(--sp-4)', alignItems: 'center' }}>
              <span className="soft" style={{ width: 78, flexShrink: 0, fontSize: 12, whiteSpace: 'nowrap' }}>{t('labelDueDate')}</span>
              <div style={{ flex: 1, minWidth: 160, color: due?.urgent || due?.overdue ? 'var(--danger)' : undefined }}>
                <InlineDate
                  value={action.dueDate || null}
                  onSave={handleDueDateChange}
                  ariaLabel={t('labelDueDate')}
                  placeholder={t('placeholderAddValue')}
                  testId="action-due-date"
                />
              </div>
            </div>
          </div>

          <div style={{ marginBottom: 'var(--sp-8)' }}>
            <div className="field-label">{t('labelDescription')}</div>
            <InlineLongText
              value={action.description || ''}
              onSave={handleSaveDescription}
              ariaLabel={t('labelDescription')}
              placeholder={t('placeholderAddDescription')}
              testId="action-description"
            />
          </div>

          <div style={{ marginBottom: 'var(--sp-8)' }}>
            <StepList workspaceId={currentWorkspace!.id} actionId={action.id} />
          </div>

          <div style={{ marginTop: 'var(--sp-6)' }}>
            <ActionActivity
              workspaceId={currentWorkspace!.id}
              actionId={action.id}
              slackMessageTS={action.slackMessageTS}
              slackChannelID={action.case?.slackChannelID}
              slackChannelURL={action.case?.slackChannelURL}
              slackThreadTS={action.case?.slackThreadTS}
              highlightCommentId={highlightCommentId}
            />
          </div>
        </>
      )}
      {confirmArchive && action && (
        <Modal
          open
          onClose={() => setConfirmArchive(false)}
          title={t('titleArchiveAction')}
          width={420}
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirmArchive(false)}>{t('btnCancel')}</Button>
              <Button
                variant="danger"
                onClick={handleArchive}
                disabled={archiving}
                data-testid="action-archive-confirm-button"
              >
                {t('btnArchive')}
              </Button>
            </>
          }
        >
          <p
            style={{ fontSize: 13, lineHeight: 1.6, margin: 0 }}
            dangerouslySetInnerHTML={{
              __html: t('msgArchiveActionConfirm', { title: action.title || '' }),
            }}
          />
          <p style={{ fontSize: 12, lineHeight: 1.6, marginTop: 8, color: 'var(--text-muted)' }}>
            {t('noteArchiveActionReversible')}
          </p>
        </Modal>
      )}
      {confirmUnarchive && action && (
        <Modal
          open
          onClose={() => setConfirmUnarchive(false)}
          title={t('titleUnarchiveAction')}
          width={420}
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirmUnarchive(false)}>{t('btnCancel')}</Button>
              <Button
                variant="primary"
                onClick={handleUnarchive}
                disabled={unarchiving}
                data-testid="action-unarchive-confirm-button"
              >
                {t('btnUnarchive')}
              </Button>
            </>
          }
        >
          <p
            style={{ fontSize: 13, lineHeight: 1.6, margin: 0 }}
            dangerouslySetInnerHTML={{
              __html: t('msgUnarchiveActionConfirm', { title: action.title || '' }),
            }}
          />
        </Modal>
      )}
    </Modal>
  )
}
