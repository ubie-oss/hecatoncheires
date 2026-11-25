import { useState } from 'react'
import { Link, useLocation, useParams } from 'react-router'
import { useQuery, useMutation } from '@apollo/client'

import Button from '../components/Button'
import FieldDisplay from '../components/fields/FieldDisplay'
import type { FieldValueDisplay } from '../components/fields/FieldValueSupplement'
import MemoFormModal from '../components/memo/MemoFormModal'
import MemoArchiveDialog from '../components/memo/MemoArchiveDialog'
import { normalizeMemoFields } from '../components/memo/memoFields'
import { IconChevLeft, IconEdit, IconWarn } from '../components/Icons'
import { useTranslation } from '../i18n'
import {
  GET_MEMO,
  GET_MEMO_CONFIGURATION,
  ARCHIVE_MEMO,
  UNARCHIVE_MEMO,
} from '../graphql/memo'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'

interface MemoFieldValue {
  fieldId: string
  value: unknown
  display?: FieldValueDisplay | null
}

interface MemoData {
  id: string
  caseID: number
  title: string
  fields: MemoFieldValue[]
  archivedAt?: string | null
  case?: { id: number; archivedAt?: string | null } | null
  createdAt: string
  updatedAt: string
}

function formatDate(iso?: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString()
}

const backLinkStyle = {
  display: 'inline-flex',
  alignItems: 'center',
  gap: 'var(--sp-1)',
  fontSize: 13,
  color: 'var(--fg-muted)',
  textDecoration: 'none',
} as const

export default function MemoDetail() {
  const { t } = useTranslation()
  const location = useLocation()
  const { workspaceId, id, memoId } = useParams<{
    workspaceId: string
    id: string
    memoId: string
  }>()
  // Case ids in URLs are positive decimal integers; reject "0x10", "1.5", etc.
  const caseIdValid = !!id && /^[0-9]+$/.test(id)
  const caseId = caseIdValid ? Number(id) : NaN
  const validParams = !!workspaceId && caseIdValid && !!memoId

  const [editOpen, setEditOpen] = useState(false)
  const [archiveConfirmOpen, setArchiveConfirmOpen] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)

  const { data, loading, error, refetch } = useQuery(GET_MEMO, {
    variables: { workspaceId, caseID: caseId, id: memoId },
    fetchPolicy: 'cache-and-network',
    skip: !validParams,
  })
  const {
    data: configData,
    loading: configLoading,
    error: configError,
    refetch: refetchConfig,
  } = useQuery(GET_MEMO_CONFIGURATION, {
    variables: { workspaceId },
    fetchPolicy: 'cache-and-network',
    skip: !validParams,
  })

  const { data: fieldConfigData } = useQuery(GET_FIELD_CONFIGURATION, {
    variables: { workspaceId },
    skip: !validParams,
  })

  // No refetchQueries: both mutations return the full memo selection set
  // (including archivedAt), so the normalized cache updates this page in
  // place, and MemoTab's cache-and-network list query refreshes on remount.
  const [archiveMemo, { loading: archiving }] = useMutation(ARCHIVE_MEMO)
  const [unarchiveMemo] = useMutation(UNARCHIVE_MEMO)

  // The router only mounts this page with :workspaceId present; without it
  // there is no workspace to link back to, so there is nothing to render.
  if (!workspaceId) return null

  // Echo the location state the case page sent us (memo filter, case-list
  // origin tab) back on every return path so the case page restores its
  // context after this detour.
  const backState = location.state as Record<string, unknown> | null

  if (!validParams) {
    return (
      <div className="h-main-inner" data-testid="memo-detail-error">
        <p style={{ margin: '0 0 var(--sp-3)', color: 'var(--color-error)', fontSize: 13 }}>
          {t('memoLoadError')}
        </p>
        <Link
          to={`/ws/${workspaceId}/cases`}
          data-testid="memo-detail-error-back-link"
          style={backLinkStyle}
        >
          <IconChevLeft size={13} />
          {t('btnBackToList')}
        </Link>
      </div>
    )
  }

  const memo: MemoData | null | undefined = data?.memo
  const memoFields = normalizeMemoFields(configData?.memoConfiguration?.fields)
  const isArchived = !!memo?.archivedAt
  // A memo of an archived Case cannot be edited either (the server rejects
  // it). Archiving / restoring the memo itself stays available.
  const caseArchived = !!memo?.case?.archivedAt
  const caseLabel: string = fieldConfigData?.fieldConfiguration?.labels?.case || 'Case'
  const caseUrl = `/ws/${workspaceId}/cases/${caseId}`

  const handleArchiveConfirm = async () => {
    setActionError(null)
    try {
      await archiveMemo({ variables: { workspaceId, caseID: caseId, id: memoId } })
      setArchiveConfirmOpen(false)
    } catch {
      setArchiveConfirmOpen(false)
      setActionError(t('memoActionError'))
    }
  }

  const handleUnarchive = async () => {
    setActionError(null)
    try {
      await unarchiveMemo({ variables: { workspaceId, caseID: caseId, id: memoId } })
    } catch {
      setActionError(t('memoActionError'))
    }
  }

  if ((loading && !data) || (configLoading && !configData)) {
    return <div className="h-main-inner muted">{t('loading')}</div>
  }

  // A failed field-configuration load must not silently render the memo
  // without its field values, so it is a full-page error like a memo error.
  if (error || configError || !memo) {
    return (
      <div className="h-main-inner" data-testid="memo-detail-error">
        <p style={{ margin: '0 0 var(--sp-3)', color: 'var(--color-error)', fontSize: 13 }}>
          {t('memoLoadError')}
        </p>
        <div className="row" style={{ gap: 'var(--sp-3)', alignItems: 'center' }}>
          <Button
            size="sm"
            onClick={() => {
              void refetch()
              void refetchConfig()
            }}
          >
            {t('memoLoadErrorRetry')}
          </Button>
          <Link
            to={caseUrl}
            state={backState}
            data-testid="memo-detail-error-back-link"
            style={backLinkStyle}
          >
            <IconChevLeft size={13} />
            {t('btnBackToCase')}
          </Link>
        </div>
      </div>
    )
  }

  return (
    <div className="h-main-inner" data-testid="memo-detail-page">
      {/* Back link */}
      <div className="row" style={{ marginBottom: 'var(--sp-3)' }}>
        <Link
          to={caseUrl}
          state={backState}
          data-testid="memo-detail-back-link"
          style={backLinkStyle}
        >
          <IconChevLeft size={13} />
          {t('btnBackToCase')}
        </Link>
      </div>

      {/* Title + actions */}
      <div className="row" style={{ alignItems: 'center', gap: 'var(--sp-2)', marginBottom: 'var(--sp-2)' }}>
        <h1
          data-testid="memo-detail-title"
          style={{ margin: 0, fontSize: 20, fontWeight: 600, minWidth: 0, overflowWrap: 'anywhere' }}
        >
          {memo.title}
        </h1>
        {isArchived && (
          <span
            style={{
              fontSize: 11,
              padding: '1px 8px',
              borderRadius: 999,
              border: '1px solid var(--border-default)',
              color: 'var(--fg-muted)',
              background: 'var(--bg-subtle)',
              flexShrink: 0,
            }}
          >
            {t('memoArchivedBadge')}
          </span>
        )}
        <span className="spacer" />
        {isArchived ? (
          <>
            <span style={{ fontSize: 12, color: 'var(--fg-muted)', flexShrink: 0 }}>
              {t('memoDetailRestoreHint')}
            </span>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => { void handleUnarchive() }}
              data-testid="memo-detail-unarchive-button"
            >
              {t('btnUnarchive')}
            </Button>
          </>
        ) : (
          <>
            {!caseArchived && (
              <Button
                variant="primary"
                size="sm"
                icon={<IconEdit size={13} />}
                onClick={() => setEditOpen(true)}
                data-testid="memo-detail-edit-button"
              >
                {t('btnEdit')}
              </Button>
            )}
            <Button
              variant="danger"
              size="sm"
              onClick={() => setArchiveConfirmOpen(true)}
              data-testid="memo-detail-archive-button"
            >
              {t('btnArchive')}
            </Button>
          </>
        )}
      </div>

      {/* Mutation failure notice */}
      {actionError && (
        <p style={{ margin: '0 0 var(--sp-2)', color: 'var(--color-error)', fontSize: 12 }}>
          {actionError}
        </p>
      )}

      {/* Archived notice */}
      {isArchived && (
        <div
          style={{
            display: 'flex',
            gap: 'var(--sp-2)',
            alignItems: 'flex-start',
            background: 'var(--bg-sunken)',
            border: '1px solid var(--border-light)',
            borderRadius: '0.375rem',
            padding: 'var(--sp-3) var(--sp-4)',
            fontSize: 12,
            color: 'var(--fg-muted)',
            lineHeight: 1.6,
            marginBottom: 'var(--sp-3)',
          }}
        >
          <IconWarn size={14} style={{ color: 'var(--warn)', flexShrink: 0, marginTop: 1 }} />
          <span>{t('memoDetailArchivedNotice')}</span>
        </div>
      )}

      {/* Archived parent case notice */}
      {!isArchived && caseArchived && (
        <div
          data-testid="memo-detail-case-archived-notice"
          style={{
            display: 'flex',
            gap: 'var(--sp-2)',
            alignItems: 'flex-start',
            background: 'var(--bg-sunken)',
            border: '1px solid var(--border-light)',
            borderRadius: '0.375rem',
            padding: 'var(--sp-3) var(--sp-4)',
            fontSize: 12,
            color: 'var(--fg-muted)',
            lineHeight: 1.6,
            marginBottom: 'var(--sp-3)',
          }}
        >
          <IconWarn size={14} style={{ color: 'var(--warn)', flexShrink: 0, marginTop: 1 }} />
          <span>{t('memoDetailCaseArchivedNotice', { caseLabel })}</span>
        </div>
      )}

      {/* Meta row */}
      <div
        style={{
          display: 'flex',
          gap: 'var(--sp-6)',
          fontSize: 12,
          color: 'var(--fg-muted)',
          flexWrap: 'wrap',
          marginBottom: 'var(--sp-4)',
        }}
      >
        <span>
          <span style={{ marginRight: 4 }}>{t('memoIdLabel')}:</span>
          <span className="mono" style={{ fontSize: 11 }}>{memo.id}</span>
        </span>
        <span>
          <span style={{ marginRight: 4 }}>{t('memoCreatedLabel')}:</span>
          <span className="mono" style={{ fontSize: 11 }}>{formatDate(memo.createdAt)}</span>
        </span>
        <span>
          <span style={{ marginRight: 4 }}>{t('memoUpdatedLabel')}:</span>
          <span className="mono" style={{ fontSize: 11 }}>{formatDate(memo.updatedAt)}</span>
        </span>
      </div>

      {/* Fields */}
      {memoFields.length > 0 && (
        <div
          className="card"
          style={{
            padding: '16px 20px',
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--sp-4)',
          }}
        >
          {memoFields.map((f) => {
            const fv = memo.fields?.find((x) => x.fieldId === f.id)
            return (
              <div key={f.id} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--sp-1)' }}>
                <span
                  style={{
                    fontSize: 11,
                    fontWeight: 600,
                    textTransform: 'uppercase',
                    letterSpacing: '0.04em',
                    color: 'var(--fg-muted)',
                  }}
                >
                  {f.name}
                </span>
                <FieldDisplay
                  field={{ ...f, options: f.options ?? undefined }}
                  value={fv?.value}
                  display={fv?.display}
                />
              </div>
            )
          })}
        </div>
      )}

      {/* Edit form modal */}
      {editOpen && !isArchived && !caseArchived && (
        <MemoFormModal
          workspaceId={workspaceId}
          caseId={caseId}
          memoId={memoId}
          memoFields={memoFields}
          onClose={() => setEditOpen(false)}
          onSaved={() => setEditOpen(false)}
          activeFilter="ACTIVE"
        />
      )}

      {/* Archive confirm dialog */}
      {archiveConfirmOpen && (
        <MemoArchiveDialog
          memoTitle={memo.title}
          onConfirm={() => { void handleArchiveConfirm() }}
          onCancel={() => setArchiveConfirmOpen(false)}
          archiving={archiving}
        />
      )}
    </div>
  )
}
