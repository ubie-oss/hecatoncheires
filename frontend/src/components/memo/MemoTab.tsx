import { useState, Fragment } from 'react'
import { Link, useLocation } from 'react-router'
import { useQuery } from '@apollo/client'
import Button from '../Button'
import { IconSparkle, IconChevRight, IconChevDown, IconPlus } from '../Icons'
import { useTranslation } from '../../i18n'
import { GET_MEMOS_BY_CASE, GET_MEMO_CONFIGURATION } from '../../graphql/memo'
import { normalizeMemoFields, type FieldDef, type FieldOption } from './memoFields'
import MemoFormModal from './MemoFormModal'

// The AI purple accent color — intentionally not a design token
const AI_PURPLE = 'oklch(0.55 0.18 290)'

interface Props {
  caseId: number
  workspaceId: string
  accessDenied?: boolean
  /** The parent Case is archived: memos are listed but none can be created
   *  (the server rejects it), so the create buttons are not shown. */
  readOnly?: boolean
}

interface MemoField {
  fieldId: string
  value: unknown
}

interface Memo {
  id: string
  caseID: number
  title: string
  fields: MemoField[]
  archivedAt?: string | null
  createdAt: string
  updatedAt: string
}

// Maps option index → color for the left rail
const RAIL_PALETTE = [
  'var(--ok)',
  'var(--accent)',
  'var(--warn)',
  AI_PURPLE,
  'var(--info)',
]

function railColor(field: FieldDef | undefined, memo: Memo): string {
  if (!field) return 'var(--fg-soft)'
  const fv = memo.fields?.find((x) => x.fieldId === field.id)
  const val = fv?.value
  if (!val || !field.options) return 'var(--fg-soft)'
  const idx = field.options.findIndex((o) => o.id === val)
  if (idx < 0) return 'var(--fg-soft)'
  return RAIL_PALETTE[idx % RAIL_PALETTE.length] ?? 'var(--fg-soft)'
}

function formatDate(iso?: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString()
}

// DefinitionBanner — collapsible description for the memo configuration
function MemoDefBanner({ description }: { description: string }) {
  const { t } = useTranslation()
  const [expanded, setExpanded] = useState(false)

  return (
    <div
      style={{
        border: '1px solid var(--border-light)',
        borderRadius: '0.375rem',
        background: 'var(--bg-subtle)',
        overflow: 'hidden',
        marginBottom: 'var(--sp-4)',
      }}
    >
      <button
        type="button"
        onClick={() => setExpanded((v) => !v)}
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 'var(--sp-2)',
          width: '100%',
          padding: 'var(--sp-3) var(--sp-4)',
          background: 'none',
          border: 'none',
          cursor: 'pointer',
          color: 'var(--fg)',
          fontFamily: 'inherit',
          fontSize: 12,
          fontWeight: 600,
          textAlign: 'left',
        }}
      >
        <IconSparkle size={13} style={{ color: AI_PURPLE, flexShrink: 0 }} />
        <span style={{ flex: 1 }}>{t('memoDefBannerTitle')}</span>
        {expanded ? <IconChevDown size={13} /> : <IconChevRight size={13} />}
        <span style={{ color: 'var(--fg-muted)', fontWeight: 400, marginLeft: 'var(--sp-1)' }}>
          {expanded ? t('memoDefBannerCollapse') : t('memoDefBannerExpand')}
        </span>
      </button>
      {expanded && (
        <div
          style={{
            padding: 'var(--sp-1) var(--sp-4) var(--sp-4)',
            fontSize: 12,
            color: 'var(--fg-muted)',
            lineHeight: 1.7,
            borderTop: '1px solid var(--border-light)',
          }}
        >
          {description}
        </div>
      )}
    </div>
  )
}

// SkeletonRow — placeholder row for loading state
function SkeletonRow() {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 'var(--sp-3)',
        padding: 'var(--sp-3) var(--sp-4)',
        border: '1px solid var(--border-light)',
        borderRadius: '0.375rem',
        background: 'var(--bg-paper)',
        marginBottom: 'var(--sp-2)',
      }}
    >
      <div style={{ width: 3, height: 40, borderRadius: 2, background: 'var(--bg-muted)' }} />
      <div style={{ flex: 1, display: 'flex', flexDirection: 'column', gap: 6 }}>
        <div style={{ height: 12, width: '45%', borderRadius: 4, background: 'var(--bg-muted)' }} />
        <div style={{ height: 10, width: '70%', borderRadius: 4, background: 'var(--bg-sunken)' }} />
      </div>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 4, alignItems: 'flex-end' }}>
        <div style={{ height: 10, width: 60, borderRadius: 4, background: 'var(--bg-sunken)' }} />
        <div style={{ height: 10, width: 60, borderRadius: 4, background: 'var(--bg-sunken)' }} />
      </div>
    </div>
  )
}

// MemoRowItem — single row in the memo list. Rendered as a real link so the
// dedicated memo URL supports open-in-new-tab / copy-link affordances.
interface MemoRowProps {
  memo: Memo
  memoFields: FieldDef[]
  summaryFields: FieldDef[]
  colorField: FieldDef | undefined
  to: string
  state: Record<string, unknown>
}

function MemoRowItem({ memo, memoFields: _memoFields, summaryFields, colorField, to, state }: MemoRowProps) {
  const { t } = useTranslation()
  const isArchived = !!memo.archivedAt
  const color = railColor(colorField, memo)

  return (
    <Link
      to={to}
      state={state}
      data-testid="memo-row"
      style={{
        display: 'flex',
        alignItems: 'stretch',
        gap: 0,
        padding: 0,
        border: '1px solid var(--border-light)',
        borderRadius: '0.375rem',
        background: 'var(--bg-paper)',
        marginBottom: 'var(--sp-2)',
        cursor: 'pointer',
        width: '100%',
        textAlign: 'left',
        color: 'inherit',
        textDecoration: 'none',
        opacity: isArchived ? 0.66 : 1,
        transition: 'border-color 0.1s',
      }}
      onMouseEnter={(e) => {
        ;(e.currentTarget as HTMLAnchorElement).style.borderColor = 'var(--border-hover)'
      }}
      onMouseLeave={(e) => {
        ;(e.currentTarget as HTMLAnchorElement).style.borderColor = 'var(--border-light)'
      }}
    >
      {/* Color rail */}
      <div
        style={{
          width: 3,
          borderRadius: '0.375rem 0 0 0.375rem',
          background: color,
          flexShrink: 0,
        }}
      />

      {/* Main content */}
      <div style={{ flex: 1, padding: 'var(--sp-3) var(--sp-3)', minWidth: 0 }}>
        {/* Title row */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--sp-2)', marginBottom: 'var(--sp-1)' }}>
          <span
            style={{
              fontSize: 13,
              fontWeight: 600,
              color: 'var(--fg)',
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
              flex: 1,
            }}
          >
            {memo.title}
          </span>
          {isArchived && (
            <span
              style={{
                fontSize: 10,
                padding: '1px 6px',
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
        </div>

        {/* Summary chips — schema-driven SELECT/MULTI_SELECT fields */}
        {summaryFields.length > 0 && (
          <div style={{ display: 'flex', gap: 'var(--sp-1)', flexWrap: 'wrap', marginTop: 'var(--sp-1)' }}>
            {summaryFields.map((f) => {
              const fv = memo.fields?.find((x) => x.fieldId === f.id)
              const val = fv?.value
              if (!val) return null

              if (f.type === 'SELECT') {
                const opt = f.options?.find((o) => o.id === val)
                if (!opt) return null
                return (
                  <span
                    key={f.id}
                    style={{
                      fontSize: 10,
                      padding: '1px 6px',
                      borderRadius: 999,
                      border: '1px solid var(--border-light)',
                      background: 'var(--bg-sunken)',
                      color: 'var(--fg-soft)',
                    }}
                  >
                    {opt.name}
                  </span>
                )
              }

              if (f.type === 'MULTI_SELECT') {
                const ids: string[] = Array.isArray(val) ? val : []
                const opts = ids
                  .map((id) => f.options?.find((o) => o.id === id))
                  .filter(Boolean) as FieldOption[]
                const shown = opts.slice(0, 2)
                const more = opts.length - shown.length
                return (
                  <Fragment key={f.id}>
                    {shown.map((o) => (
                      <span
                        key={`${f.id}-${o.id}`}
                        style={{
                          fontSize: 10,
                          padding: '1px 6px',
                          borderRadius: 999,
                          border: '1px solid var(--border-light)',
                          background: 'var(--bg-sunken)',
                          color: 'var(--fg-soft)',
                        }}
                      >
                        {o.name}
                      </span>
                    ))}
                    {more > 0 && (
                      <span
                        key={`${f.id}-more`}
                        style={{
                          fontSize: 10,
                          padding: '1px 6px',
                          borderRadius: 999,
                          border: '1px solid var(--border-light)',
                          background: 'var(--bg-sunken)',
                          color: 'var(--fg-muted)',
                        }}
                      >
                        +{more}
                      </span>
                    )}
                  </Fragment>
                )
              }

              return null
            })}
          </div>
        )}
      </div>

      {/* Right column: timestamps */}
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          justifyContent: 'center',
          alignItems: 'flex-end',
          gap: 'var(--sp-1)',
          padding: 'var(--sp-3)',
          flexShrink: 0,
        }}
      >
        <span style={{ fontSize: 10, color: 'var(--fg-muted)', whiteSpace: 'nowrap' }}>
          {t('memoUpdatedLabel')} {formatDate(memo.updatedAt)}
        </span>
        <span style={{ fontSize: 10, color: 'var(--fg-muted)', whiteSpace: 'nowrap' }}>
          {t('memoCreatedLabel')} {formatDate(memo.createdAt)}
        </span>
        <IconChevRight size={13} style={{ color: 'var(--fg-soft)', marginTop: 2 }} />
      </div>
    </Link>
  )
}

export default function MemoTab({ caseId, workspaceId, accessDenied, readOnly = false }: Props) {
  const { t } = useTranslation()
  const location = useLocation()

  type FilterValue = 'ACTIVE' | 'ARCHIVED' | null
  // Restore the filter that was active when the user navigated into a memo
  // detail page: the memo page echoes this component's location state back on
  // its return links. `null` means the ALL filter, so the state uses an 'ALL'
  // sentinel to stay distinguishable from "no state".
  const [filterValue, setFilterValue] = useState<FilterValue>(() => {
    const s = (location.state as { memoFilter?: string } | null)?.memoFilter
    if (s === 'ARCHIVED') return 'ARCHIVED'
    if (s === 'ALL') return null
    return 'ACTIVE'
  })
  const [showCreateForm, setShowCreateForm] = useState(false)

  const { data: configData } = useQuery(GET_MEMO_CONFIGURATION, {
    variables: { workspaceId },
    fetchPolicy: 'cache-and-network',
  })

  const { data, loading, error, refetch } = useQuery(GET_MEMOS_BY_CASE, {
    variables: { workspaceId, caseID: caseId, filter: filterValue },
    fetchPolicy: 'cache-and-network',
    skip: accessDenied,
  })

  if (accessDenied) return null

  const memoConfig = configData?.memoConfiguration
  const memoFields: FieldDef[] = normalizeMemoFields(memoConfig?.fields)
  const memos: Memo[] = data?.memosByCase ?? []

  // Summary fields: first 2-3 SELECT or MULTI_SELECT fields in schema order
  const summaryFields = memoFields
    .filter((f) => f.type === 'SELECT' || f.type === 'MULTI_SELECT')
    .slice(0, 3)

  // Color field: first SELECT field in schema order
  const colorField = memoFields.find((f) => f.type === 'SELECT')

  return (
    <div>
      {/* Definition banner */}
      {memoConfig?.description && (
        <MemoDefBanner description={memoConfig.description} />
      )}

      {/* Filter bar */}
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 'var(--sp-3)',
          marginBottom: 'var(--sp-4)',
        }}
      >
        <div
          className="seg-toggle"
          role="tablist"
          aria-label="Memo filter"
        >
          <button
            type="button"
            role="tab"
            data-testid="memo-filter-active"
            aria-selected={filterValue === 'ACTIVE'}
            className={filterValue === 'ACTIVE' ? 'seg-toggle-btn seg-toggle-btn--active' : 'seg-toggle-btn'}
            onClick={() => setFilterValue('ACTIVE')}
          >
            {t('memoFilterActive')}
          </button>
          <button
            type="button"
            role="tab"
            data-testid="memo-filter-archived"
            aria-selected={filterValue === 'ARCHIVED'}
            className={filterValue === 'ARCHIVED' ? 'seg-toggle-btn seg-toggle-btn--active' : 'seg-toggle-btn'}
            onClick={() => setFilterValue('ARCHIVED')}
          >
            {t('memoFilterArchived')}
          </button>
          <button
            type="button"
            role="tab"
            data-testid="memo-filter-all"
            aria-selected={filterValue === null}
            className={filterValue === null ? 'seg-toggle-btn seg-toggle-btn--active' : 'seg-toggle-btn'}
            onClick={() => setFilterValue(null)}
          >
            {t('memoFilterAll')}
          </button>
        </div>

        {!loading && memos.length > 0 && (
          <span style={{ fontSize: 12, color: 'var(--fg-muted)' }}>
            {t('memoCountLabel', { count: memos.length })}
          </span>
        )}

        <span style={{ flex: 1 }} />

        {!readOnly && (
          <Button
            size="sm"
            icon={<IconPlus size={12} />}
            onClick={() => setShowCreateForm(true)}
            data-testid="new-memo-button"
          >
            {t('btnNewMemo')}
          </Button>
        )}
      </div>

      {/* Loading skeleton */}
      {loading && (
        <>
          <SkeletonRow />
          <SkeletonRow />
          <SkeletonRow />
        </>
      )}

      {/* Error state */}
      {!loading && error && (
        <div
          style={{
            padding: 'var(--sp-4)',
            textAlign: 'center',
            color: 'var(--color-error)',
            fontSize: 13,
          }}
        >
          <p style={{ margin: '0 0 var(--sp-3)' }}>{t('memoLoadError')}</p>
          <Button size="sm" onClick={() => { void refetch() }}>
            {t('memoLoadErrorRetry')}
          </Button>
        </div>
      )}

      {/* Empty state */}
      {!loading && !error && memos.length === 0 && (
        <div
          style={{
            padding: 'var(--sp-8)',
            textAlign: 'center',
            border: '1px solid var(--border-light)',
            borderRadius: '0.375rem',
            background: 'var(--bg-paper)',
          }}
        >
          <h3 style={{ margin: '0 0 6px', fontSize: 14 }}>
            {filterValue === 'ARCHIVED' ? t('memoEmptyArchivedTitle') : t('memoEmptyActiveTitle')}
          </h3>
          <p style={{ margin: '0 0 var(--sp-4)', fontSize: 12, color: 'var(--fg-muted)' }}>
            {filterValue === 'ARCHIVED' ? t('memoEmptyArchivedDesc') : t('memoEmptyActiveDesc')}
          </p>
          {filterValue !== 'ARCHIVED' && !readOnly && (
            <Button
              size="sm"
              icon={<IconPlus size={12} />}
              onClick={() => setShowCreateForm(true)}
            >
              {t('btnNewMemo')}
            </Button>
          )}
        </div>
      )}

      {/* Memo list */}
      {!loading && !error && memos.length > 0 && (
        <div>
          {memos.map((memo) => (
            <MemoRowItem
              key={memo.id}
              memo={memo}
              memoFields={memoFields}
              summaryFields={summaryFields}
              colorField={colorField}
              to={`/ws/${workspaceId}/cases/${caseId}/memos/${memo.id}`}
              state={{
                ...((location.state as Record<string, unknown> | null) ?? {}),
                memoFilter: filterValue ?? 'ALL',
              }}
            />
          ))}
        </div>
      )}

      {/* Create form modal (editing happens on the memo detail page) */}
      {showCreateForm && !readOnly && (
        <MemoFormModal
          workspaceId={workspaceId}
          caseId={caseId}
          memoFields={memoFields}
          onClose={() => setShowCreateForm(false)}
          onSaved={() => setShowCreateForm(false)}
          activeFilter={filterValue}
        />
      )}
    </div>
  )
}
