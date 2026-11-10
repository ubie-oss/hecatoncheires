import { useMemo, useState } from 'react'
import { Navigate, useNavigate, useParams, useLocation } from 'react-router'
import { useMutation, useQuery } from '@apollo/client'
import { BULK_ARCHIVE_ACTIONS, GET_ACTIONS_BY_CASE, GET_OPEN_CASE_ACTIONS, UPDATE_ACTION } from '../graphql/action'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'
import { GET_CASES } from '../graphql/case'
import { useWorkspace } from '../contexts/workspace-context'
import { useTranslation } from '../i18n'
import { useActionStatuses } from '../hooks/useActionStatuses'
import { useCaseStatuses } from '../hooks/useCaseStatuses'
import { useActionsEnabled } from '../hooks/useActionsEnabled'
import {
  actionStatusColorStyle,
  actionStatusSlug,
} from '../utils/actionStatusStyle'
import Button from '../components/Button'
import Modal from '../components/Modal'
import {
  IconDots,
  IconPlus,
  IconSearch,
} from '../components/Icons'
import { Avatar } from '../components/Primitives'
import CaseFilterSelect from '../components/CaseFilterSelect'
import { activateOnEnterOrSpace } from '../utils/keyboard'
import ActionForm from './ActionForm'
import ActionModal from './ActionModal'
import CaseFieldFilters from '../components/CaseFieldFilters'
import { useCaseFieldFilters } from '../hooks/useCaseFieldFilters'
import { matchesCaseFields, type CaseFieldDefinition } from '../utils/caseFieldFilters'
import styles from './ActionList.module.css'

interface ActionRow {
  id: number
  caseID: number
  case?: { id: number; title: string; accessDenied?: boolean; fields?: { fieldId: string; value: unknown }[] }
  title: string
  description: string
  assigneeID: string | null
  assignee: { id: string; name: string; realName: string; imageUrl?: string } | null
  status: string
  dueDate?: string | null
  createdAt: string
  stepProgress?: { done: number; total: number }
}

function formatDue(iso?: string | null) {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  const today = new Date()
  const diff = Math.round((d.getTime() - today.getTime()) / (1000 * 60 * 60 * 24))
  if (diff < 0) return 'Overdue'
  if (diff === 0) return 'Today'
  if (diff === 1) return 'Tomorrow'
  return d.toLocaleDateString()
}

export default function ActionList() {
  const navigate = useNavigate()
  const { search: queryString } = useLocation()
  const { actionId, caseId } = useParams<{ actionId?: string; caseId?: string }>()
  const { currentWorkspace } = useWorkspace()
  const { t } = useTranslation()

  const fieldFilter = useCaseFieldFilters()
  const [search, setSearch] = useState('')
  const [showCreate, setShowCreate] = useState(false)
  const [draggingId, setDraggingId] = useState<number | null>(null)
  const [dragOverCol, setDragOverCol] = useState<string | null>(null)
  // Column whose kebab menu is open, and the column pending bulk-archive
  // confirmation. Both are status ids (null = none).
  const [menuCol, setMenuCol] = useState<string | null>(null)
  const [confirmArchiveCol, setConfirmArchiveCol] = useState<string | null>(null)
  // Bulk archive is dispatched asynchronously server-side, so the immediate
  // refetch still returns these actions as active. Hide them optimistically by
  // id until they fall out of the board for real.
  const [archivingIds, setArchivingIds] = useState<Set<number>>(new Set())
  const { statuses, isClosed, label } = useActionStatuses(currentWorkspace?.id)
  // A thread-mode workspace has Actions only when it enables them; without
  // them its /actions URL (which used to render the Case board) forwards to
  // the Case board at /board.
  const { isThreadMode } = useCaseStatuses(currentWorkspace?.id)
  const actionsEnabled = useActionsEnabled(currentWorkspace?.id)

  const filterCaseId = useMemo(() => {
    if (!caseId) return null
    const n = Number(caseId)
    return Number.isFinite(n) && n > 0 ? n : null
  }, [caseId])

  const rootUrl = useMemo(() => {
    return currentWorkspace ? `/ws/${currentWorkspace.id}/actions` : ''
  }, [currentWorkspace])

  const baseUrl = useMemo(() => {
    if (!rootUrl) return ''
    return filterCaseId != null ? `${rootUrl}/case/${filterCaseId}` : rootUrl
  }, [rootUrl, filterCaseId])

  const { data: openData } = useQuery(GET_OPEN_CASE_ACTIONS, {
    variables: { workspaceId: currentWorkspace?.id },
    skip: !currentWorkspace || filterCaseId != null,
  })
  const { data: byCaseData } = useQuery(GET_ACTIONS_BY_CASE, {
    variables: { workspaceId: currentWorkspace?.id, caseID: filterCaseId ?? 0 },
    skip: !currentWorkspace || filterCaseId == null,
  })
  const { data: configData } = useQuery(GET_FIELD_CONFIGURATION, {
    variables: { workspaceId: currentWorkspace?.id },
    skip: !currentWorkspace,
  })
  const { data: casesData } = useQuery(GET_CASES, {
    variables: { workspaceId: currentWorkspace?.id, status: 'OPEN' },
    skip: !currentWorkspace,
  })
  const fieldDefs: CaseFieldDefinition[] = useMemo(() => configData?.fieldConfiguration?.fields ?? [], [configData])
  const caseLabel = configData?.fieldConfiguration?.labels?.case || 'Case'
  const refetchActionsQuery = useMemo(
    () =>
      filterCaseId != null
        ? { query: GET_ACTIONS_BY_CASE, variables: { workspaceId: currentWorkspace?.id, caseID: filterCaseId } }
        : { query: GET_OPEN_CASE_ACTIONS, variables: { workspaceId: currentWorkspace?.id } },
    [filterCaseId, currentWorkspace?.id],
  )
  const [updateAction] = useMutation(UPDATE_ACTION, {
    refetchQueries: [refetchActionsQuery],
  })
  // No refetchQueries: the server archives asynchronously, so an immediate
  // refetch cannot reflect completion. The optimistic archivingIds set is what
  // removes the cards; the board reconciles for real on the next natural load.
  const [bulkArchiveActions, { loading: bulkArchiving }] = useMutation(BULK_ARCHIVE_ACTIONS)

  const actions: ActionRow[] = useMemo(() => {
    if (filterCaseId != null) return byCaseData?.actionsByCase || []
    return openData?.openCaseActions || []
  }, [filterCaseId, byCaseData, openData])

  const filterCases = useMemo(() => actions.map((a) => a.case), [actions])

  const openCases = useMemo(
    () =>
      (casesData?.cases ?? []).map((c: { id: number; title: string }) => ({
        id: c.id,
        title: c.title,
      })),
    [casesData],
  )

  // Cases not present in the OPEN list (e.g. CLOSED / DRAFT cases the user
  // landed on via a direct URL) still need to render in the dropdown so the
  // selected value isn't "All". Fall back to the case info attached to the
  // first action returned by GET_ACTIONS_BY_CASE.
  const extraOption = useMemo(() => {
    if (filterCaseId == null) return null
    if (openCases.some((c: { id: number }) => c.id === filterCaseId)) return null
    const fromActions = actions.find((a) => a.case)?.case
    if (fromActions) return { id: fromActions.id, title: fromActions.title }
    return { id: filterCaseId, title: '' }
  }, [filterCaseId, openCases, actions])
  const filtered = useMemo(() => {
    const visible = archivingIds.size === 0
      ? actions
      : actions.filter((a) => !archivingIds.has(a.id))
    const matching = visible.filter((a) => matchesCaseFields(a.case, fieldFilter.filters, fieldDefs))
    if (!search.trim()) return matching
    const q = search.toLowerCase()
    return matching.filter((a) =>
      a.title.toLowerCase().includes(q) ||
      (a.description || '').toLowerCase().includes(q) ||
      (a.case?.title || '').toLowerCase().includes(q),
    )
  }, [actions, search, archivingIds, fieldFilter.filters, fieldDefs])

  const grouped = useMemo(() => {
    const map: Record<string, ActionRow[]> = {}
    statuses.forEach((s) => { map[s.id] = [] })
    filtered.forEach((a) => {
      if (!map[a.status]) map[a.status] = []
      map[a.status].push(a)
    })
    return map
  }, [filtered, statuses])

  const detailActionId = actionId ? Number(actionId) : null

  const handleDrop = async (target: string) => {
    const id = draggingId
    setDraggingId(null)
    setDragOverCol(null)
    if (id == null) return
    const a = actions.find((x) => x.id === id)
    if (!a || a.status === target) return
    try {
      await updateAction({
        variables: { workspaceId: currentWorkspace!.id, input: { id, status: target } },
        optimisticResponse: {
          updateAction: { ...a, status: target, __typename: 'Action' },
        },
      })
    } catch (e) {
      console.error('Failed to move action', e)
    }
  }

  const handleBulkArchive = async (colId: string) => {
    if (!currentWorkspace) return
    const ids = (grouped[colId] ?? []).map((a) => a.id)
    setConfirmArchiveCol(null)
    if (ids.length === 0) return
    // Optimistically hide the cards: the server archives asynchronously, so an
    // immediate refetch would still return them as active.
    setArchivingIds((prev) => new Set([...prev, ...ids]))
    try {
      await bulkArchiveActions({
        variables: { workspaceId: currentWorkspace.id, ids },
      })
    } catch (e) {
      // The dispatch itself was rejected (e.g. auth / network); un-hide so the
      // user sees the actions are still there rather than silently losing them.
      console.error('Failed to bulk archive actions', e)
      setArchivingIds((prev) => {
        const next = new Set(prev)
        ids.forEach((id) => next.delete(id))
        return next
      })
    }
  }

  const openCount = filtered.filter((a) => !isClosed(a.status)).length

  if (isThreadMode && actionsEnabled === false && currentWorkspace) {
    return <Navigate to={`/ws/${currentWorkspace.id}/board${queryString}`} replace />
  }

  return (
    <div className="h-main-inner" style={{ display: 'flex', flexDirection: 'column' }}>
      <div className="h-page-h">
        <div>
          <h1>{t('titleActions', { workspaceName: currentWorkspace?.name || '' })}</h1>
          <div className="sub">{t('subtitleActions')} · {openCount} open</div>
        </div>
        <div className="actions">
          <Button variant="primary" icon={<IconPlus size={14} />} onClick={() => setShowCreate(true)}>
            {t('btnNewAction')}
          </Button>
        </div>
      </div>

      <div className="row" style={{ marginBottom: 12, gap: 12, flexWrap: 'wrap', alignItems: 'center' }}>
        <CaseFilterSelect
          cases={openCases}
          selectedCaseId={filterCaseId}
          onSelect={(id) => {
            if (!rootUrl) return
            if (id == null) navigate(`${rootUrl}${queryString}`)
            else navigate(`${rootUrl}/case/${id}${queryString}`)
          }}
          caseLabel={t('labelCaseFilter', { caseLabel })}
          allLabel={t('filterAllCases', { caseLabel })}
          searchPlaceholder={t('placeholderSearchCases')}
          emptyLabel={t('emptyCaseSearch')}
          triggerAriaLabel={t('ariaSelectCaseFilter', { caseLabel })}
          searchAriaLabel={t('ariaSearchCases', { caseLabel })}
          extraOption={extraOption}
          testId="action-case-filter"
        />
        <CaseFieldFilters key={currentWorkspace?.id} fields={fieldDefs} cases={filterCases} {...fieldFilter} />
        <div className="h-search" style={{ width: 280, marginLeft: 0 }}>
          <IconSearch size={13} />
          <input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t('placeholderSearchActions')}
            data-testid="action-search-input"
            style={{
              flex: 1, border: 'none', background: 'transparent', outline: 'none',
              fontFamily: 'inherit', color: 'var(--fg)', fontSize: 12.5,
            }}
          />
        </div>
        {search && (
          <Button
            size="sm"
            variant="ghost"
            onClick={() => setSearch('')}
            data-testid="action-filter-clear"
          >
            {t('btnClear')}
          </Button>
        )}
      </div>

      <div
        data-testid="kanban-board"
        className={`kanban ${styles.kanbanWrap}`}
      >
        {statuses.map((col) => (
          <div
            key={col.id}
            className="kan-col"
            data-testid={`kanban-column-${actionStatusSlug(label(col.id))}`}
            onDragOver={(e) => { e.preventDefault(); if (dragOverCol !== col.id) setDragOverCol(col.id) }}
            onDragLeave={() => { if (dragOverCol === col.id) setDragOverCol(null) }}
            onDrop={(e) => { e.preventDefault(); handleDrop(col.id) }}
            style={dragOverCol === col.id ? { outline: '2px dashed var(--accent)', outlineOffset: -2 } : undefined}
          >
            <div className="kan-h">
              <span
                className="pip"
                style={{ width: 8, height: 8, borderRadius: '50%', ...actionStatusColorStyle(col.color) }}
              />
              {label(col.id)}
              <span className="count">{(grouped[col.id] ?? []).length}</span>
              {/* Bulk-archive affordance lives only on completed (closed)
                  columns — that is where finished actions pile up. */}
              {isClosed(col.id) && (
                <div className={styles.columnMenuWrap}>
                  <button
                    type="button"
                    className={styles.columnMenuButton}
                    aria-label={t('ariaColumnMenu', { status: label(col.id) })}
                    aria-haspopup="menu"
                    aria-expanded={menuCol === col.id}
                    data-testid={`kanban-column-menu-${actionStatusSlug(label(col.id))}`}
                    onClick={() => setMenuCol((v) => (v === col.id ? null : col.id))}
                  >
                    <IconDots size={14} />
                  </button>
                  {menuCol === col.id && (
                    <>
                      <div
                        aria-hidden="true"
                        onClick={() => setMenuCol(null)}
                        style={{ position: 'fixed', inset: 0, zIndex: 100 }}
                      />
                      <div role="menu" className={styles.kebabMenu}>
                        <button
                          type="button"
                          role="menuitem"
                          className={styles.kebabItem}
                          disabled={(grouped[col.id] ?? []).length === 0}
                          data-testid={`kanban-column-archive-all-${actionStatusSlug(label(col.id))}`}
                          onClick={() => { setMenuCol(null); setConfirmArchiveCol(col.id) }}
                        >
                          {t('menuArchiveColumnActions')}
                        </button>
                      </div>
                    </>
                  )}
                </div>
              )}
            </div>
            <div className="kan-list">
              {(grouped[col.id] ?? []).map((a) => {
                const openModal = () => navigate(`${baseUrl}/${a.id}${queryString}`)
                return (
                  <div
                    key={a.id}
                    role="button"
                    tabIndex={0}
                    className="kan-card"
                    data-testid="action-card"
                    draggable
                    onDragStart={(e) => {
                      setDraggingId(a.id)
                      e.dataTransfer.effectAllowed = 'move'
                      e.dataTransfer.setData('text/plain', String(a.id))
                    }}
                    onDragEnd={() => { setDraggingId(null); setDragOverCol(null) }}
                    onClick={openModal}
                    onKeyDown={activateOnEnterOrSpace(openModal)}
                    style={{ textAlign: 'left', opacity: draggingId === a.id ? 0.4 : 1, cursor: draggingId === a.id ? 'grabbing' : 'grab' }}
                  >
                    {a.case && (
                      <button
                        type="button"
                        className={`case-link ${styles.caseLinkButton}`}
                        data-testid="action-card-case-link"
                        aria-label={t('ariaFilterByCase', { caseLabel, id: String(a.case.id) })}
                        onClick={(e) => {
                          e.stopPropagation()
                          if (filterCaseId === a.case!.id) return
                          navigate(`${rootUrl}/case/${a.case!.id}${queryString}`)
                        }}
                      >
                        #{a.case.id} {a.case.title}
                      </button>
                    )}
                    <span className={`title ${styles.titleText}`}>{a.title}</span>
                    <div className="meta">
                      {a.assignee
                        ? <Avatar size="sm" name={a.assignee.name} realName={a.assignee.realName} imageUrl={a.assignee.imageUrl} />
                        : <span style={{ width: 20 }} />
                      }
                      {a.stepProgress && a.stepProgress.total > 0 && (
                        <span
                          className={styles.stepBadge}
                          data-testid="action-card-step-progress"
                          title={`${a.stepProgress.done}/${a.stepProgress.total} steps done`}
                        >
                          {a.stepProgress.done}/{a.stepProgress.total}
                        </span>
                      )}
                      <span className="spacer" />
                      <span className="mono" style={{ fontSize: 11 }}>{formatDue(a.dueDate)}</span>
                    </div>
                  </div>
                )
              })}
            </div>
          </div>
        ))}
      </div>

      {showCreate && (
        <ActionForm onClose={() => setShowCreate(false)} action={null} />
      )}

      {confirmArchiveCol != null && (
        <Modal
          open
          onClose={() => setConfirmArchiveCol(null)}
          title={t('titleArchiveColumnActions')}
          width={460}
          footer={
            <>
              <Button variant="ghost" onClick={() => setConfirmArchiveCol(null)}>{t('btnCancel')}</Button>
              <Button
                variant="primary"
                onClick={() => handleBulkArchive(confirmArchiveCol)}
                disabled={bulkArchiving}
                data-testid="confirm-bulk-archive-button"
              >
                {t('btnArchive')}
              </Button>
            </>
          }
        >
          <div
            style={{ fontSize: 13, lineHeight: 1.6 }}
            dangerouslySetInnerHTML={{
              __html: t('msgArchiveColumnActionsConfirm', {
                count: String((grouped[confirmArchiveCol] ?? []).length),
                status: escapeHtml(label(confirmArchiveCol)),
              }),
            }}
          />
        </Modal>
      )}

      {detailActionId && (
        <ActionModal
          actionId={detailActionId}
          onClose={() => navigate(`${baseUrl}${queryString}`)}
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
