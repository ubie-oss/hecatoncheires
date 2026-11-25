import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router'
import { useMutation, useQuery } from '@apollo/client'

import {
  GET_CASE_AGENT_SETTINGS,
  GET_CASE_JOB_RUN_LOGS,
  GET_CASE_JOBS,
  TRIGGER_CASE_JOB,
  UPDATE_CASE_AGENT_SETTINGS,
} from '../graphql/caseAgent'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'
import { useTranslation } from '../i18n'
import { runTriggerLabelKey } from '../utils/agentTrigger'
import {
  RUN_POLL_INTERVAL_MS,
  RUN_POLL_MAX_MS,
  shouldPollRunLogs,
} from '../utils/runPolling'
import {
  IconChevDown,
  IconChevLeft,
  IconChevRight,
  IconEdit,
  IconExt,
  IconRobot,
  IconWarn,
} from '../components/Icons'
import Button from '../components/Button'
import CaseJobList, { type CaseJob } from '../components/caseAgent/CaseJobList'
import Checkbox from '../components/caseAgent/Checkbox'
import MarkdownView from '../components/caseAgent/MarkdownView'
import SourceIcon from '../components/caseAgent/SourceIcon'
import StageBadge, { type JobRunStage } from '../components/caseAgent/StageBadge'
import styles from './CaseAgent.module.css'

const PROMPT_LIMIT = 16384

interface AgentSource {
  id: string
  name: string
  sourceType: string
  description: string
  enabled: boolean
}

const SOURCE_TYPE_LABEL: Record<string, string> = {
  SLACK: 'Slack',
  GITHUB: 'GitHub',
  NOTION_DB: 'Notion DB',
  NOTION_PAGE: 'Notion Page',
}

interface CaseAgentSettings {
  id: number
  title: string
  status: string
  isPrivate: boolean
  accessDenied: boolean
  archivedAt?: string | null
  slackChannelID: string | null
  slackChannelURL: string | null
  agentAdditionalPrompt: string
  agentSources: AgentSource[]
}

type JobStrategy = 'SIMPLE' | 'PLANEXEC'

interface JobRunLogRow {
  workspaceId: string
  caseId: number
  jobId: string
  jobName: string
  strategy: JobStrategy
  runId: string
  stage: JobRunStage
  startedAt: string
  endedAt: string | null
  durationMs: number | null
  errorMessage: string
  eventType: string
}

interface JobRunLogConnection {
  items: JobRunLogRow[]
  nextCursor: string | null
}

function formatDuration(ms: number | null, stage: JobRunStage, runningLabel: string): string {
  if (stage === 'RUNNING' || ms == null) return runningLabel
  if (ms < 1000) return `${ms}ms`
  const secs = Math.round(ms / 100) / 10
  if (secs < 60) return `${secs}s`
  const m = Math.floor(secs / 60)
  const rem = Math.round(secs - m * 60)
  return `${m}m ${rem}s`
}

function formatStarted(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

export default function CaseAgent() {
  const { workspaceId, id } = useParams<{ workspaceId: string; id: string }>()
  const caseId = id ? parseInt(id, 10) : 0
  const { t } = useTranslation()

  const { data, loading, error, refetch } = useQuery<{
    case: CaseAgentSettings | null
    sources: AgentSource[]
  }>(GET_CASE_AGENT_SETTINGS, {
    variables: { workspaceId, caseId },
    skip: !workspaceId || !caseId,
    fetchPolicy: 'cache-and-network',
  })
  const { data: fieldConfigData } = useQuery(GET_FIELD_CONFIGURATION, {
    variables: { workspaceId },
    skip: !workspaceId,
  })
  const caseLabel: string = fieldConfigData?.fieldConfiguration?.labels?.case || 'Case'

  // Pagination is held in a stack of "after" cursors. Index 0 is the
  // head (after=null); each Next push appends the connection's
  // nextCursor; Prev pops. We re-query with cursorStack[currentPage]
  // so navigation works in both directions without relying on Apollo
  // cache merge — the server is the source of truth for ordering.
  const [cursorStack, setCursorStack] = useState<(string | null)[]>([null])
  const [currentPage, setCurrentPage] = useState(0)
  const currentCursor = cursorStack[currentPage] ?? null

  const {
    data: runLogData,
    loading: runLogLoading,
    startPolling: startRunLogPolling,
    stopPolling: stopRunLogPolling,
  } = useQuery<{ caseJobRunLogs: JobRunLogConnection }>(GET_CASE_JOB_RUN_LOGS, {
    variables: { workspaceId, caseId, first: 20, after: currentCursor },
    skip: !workspaceId || !caseId,
    fetchPolicy: 'cache-and-network',
  })

  const {
    data: jobsData,
    loading: jobsLoading,
    error: jobsError,
    refetch: refetchJobs,
  } = useQuery<{ caseJobs: CaseJob[] }>(GET_CASE_JOBS, {
    variables: { workspaceId, caseId },
    skip: !workspaceId || !caseId,
    fetchPolicy: 'cache-and-network',
  })

  const [updateSettings, updateState] = useMutation(UPDATE_CASE_AGENT_SETTINGS, {
    refetchQueries: [
      { query: GET_CASE_AGENT_SETTINGS, variables: { workspaceId, caseId } },
    ],
  })

  const [triggerJob, triggerState] = useMutation(TRIGGER_CASE_JOB)
  // The job id whose trigger mutation is in flight. Cleared as soon as the
  // mutation settles — from then on the run-log rows say whether it runs.
  const [pendingJobId, setPendingJobId] = useState<string | null>(null)
  // Epoch ms until which a just-issued trigger keeps the page polling even
  // though no RUNNING row has appeared yet. 0 = nothing pending. Held as
  // state, not a ref, so reaching the deadline re-evaluates the polling
  // effect — a poll that returns unchanged data does not re-render, so
  // nothing else would.
  const [pollDeadline, setPollDeadline] = useState(0)

  const [editingPrompt, setEditingPrompt] = useState(false)
  const [draftPrompt, setDraftPrompt] = useState('')

  const [editingSources, setEditingSources] = useState(false)
  const [draftSourceIds, setDraftSourceIds] = useState<string[]>([])

  const caseData = data?.case
  const sources = data?.sources ?? []
  const jobs = jobsData?.caseJobs ?? []
  const runLogs = runLogData?.caseJobRunLogs?.items ?? []
  const nextCursor = runLogData?.caseJobRunLogs?.nextCursor ?? null

  // The agentSources field is already filtered server-side to the
  // operator's selection, so use it directly. The earlier no-op
  // Set→filter wrapper has been removed.
  const selectedSources: AgentSource[] = caseData?.agentSources ?? []

  // Jobs with a run in flight. The Run button for these is disabled — the
  // backend refuses a second run anyway, so the UI says so up front.
  const runningJobIds = new Set(
    runLogs.filter((r) => r.stage === 'RUNNING').map((r) => r.jobId),
  )
  const hasRunningRun = runningJobIds.size > 0

  // Start and stop polling from one place, so the page follows a run it did
  // not start (opened while a run was already in flight) as well as one it
  // did. Reading the clock here (an effect, not render) keeps the rendered
  // output a pure function of state.
  useEffect(() => {
    const now = Date.now()
    if (!shouldPollRunLogs({ rows: runLogs, page: currentPage, deadline: pollDeadline, now })) {
      stopRunLogPolling()
      if (pollDeadline !== 0) setPollDeadline(0)
      return
    }
    startRunLogPolling(RUN_POLL_INTERVAL_MS)

    // A visible RUNNING row keeps polling alive on its own, so the deadline
    // that covered the wait for it has done its job. Dropping it here is what
    // makes polling stop the moment that run reaches a terminal stage,
    // instead of lingering for the rest of the window.
    if (hasRunningRun) {
      if (pollDeadline !== 0) setPollDeadline(0)
      return
    }
    if (pollDeadline > now) {
      const timer = setTimeout(() => setPollDeadline(0), pollDeadline - now)
      return () => clearTimeout(timer)
    }
  }, [
    runLogs,
    hasRunningRun,
    currentPage,
    pollDeadline,
    startRunLogPolling,
    stopRunLogPolling,
  ])

  if (!workspaceId || !caseId) {
    return null
  }

  if (loading && !caseData) {
    return (
      <div className={styles.shell}>
        <div className="card statusCard" style={{ padding: 24 }}>
          <div className="soft">…</div>
        </div>
      </div>
    )
  }

  if (error) {
    return (
      <div className={styles.shell}>
        <div className="card" style={{ padding: 24, textAlign: 'center' }}>
          <div className="soft">{t('caseAgentLoadError')}</div>
          <div style={{ marginTop: 12 }}>
            <Button size="sm" onClick={() => void refetch()}>{t('btnSync')}</Button>
          </div>
        </div>
      </div>
    )
  }

  if (!caseData || caseData.accessDenied) {
    return (
      <div className={styles.shell}>
        <div className="card" style={{ padding: 24, textAlign: 'center' }}>
          <div className="soft">{t('caseAgentAccessDenied')}</div>
        </div>
      </div>
    )
  }

  // An archived case's agent settings are read-only: the server rejects
  // updateCaseAgentSettings, so neither Edit button is offered.
  const settingsReadOnly = !!caseData.archivedAt

  const handleStartPromptEdit = () => {
    setDraftPrompt(caseData.agentAdditionalPrompt)
    setEditingPrompt(true)
  }
  const handleCancelPromptEdit = () => {
    setEditingPrompt(false)
    setDraftPrompt('')
  }
  const handleSavePrompt = async () => {
    const next = draftPrompt
    try {
      await updateSettings({
        variables: {
          workspaceId,
          input: {
            caseId,
            agentAdditionalPrompt: next,
            enabledSourceIds: caseData.agentSources.map((s) => s.id),
          },
        },
      })
      setEditingPrompt(false)
    } catch {
      // Apollo surfaces the error via updateState.error; no extra work.
    }
  }

  const handleStartSourcesEdit = () => {
    setDraftSourceIds(caseData.agentSources.map((s) => s.id))
    setEditingSources(true)
  }
  const handleCancelSourcesEdit = () => {
    setEditingSources(false)
    setDraftSourceIds([])
  }
  const handleToggleSource = (id: string) => {
    setDraftSourceIds((curr) =>
      curr.includes(id) ? curr.filter((x) => x !== id) : [...curr, id],
    )
  }
  const handleSaveSources = async () => {
    try {
      await updateSettings({
        variables: {
          workspaceId,
          input: {
            caseId,
            agentAdditionalPrompt: caseData.agentAdditionalPrompt,
            enabledSourceIds: draftSourceIds,
          },
        },
      })
      setEditingSources(false)
    } catch {
      // surfaced via updateState
    }
  }

  const handleRunJob = async (jobId: string) => {
    setPendingJobId(jobId)
    try {
      await triggerJob({ variables: { workspaceId, caseId, jobId } })
      // The new run lands at the head of the history, so return there and
      // watch for it. Setting the deadline is enough — the polling effect
      // picks it up. RUN_POLL_MAX_MS bounds the wait if the run finishes
      // between two polls and its RUNNING row is never observed.
      setCursorStack([null])
      setCurrentPage(0)
      setPollDeadline(Date.now() + RUN_POLL_MAX_MS)
    } catch {
      // Apollo surfaces the failure via triggerState.error; the banner below
      // renders it.
    } finally {
      setPendingJobId(null)
    }
  }

  const promptLength = caseData.agentAdditionalPrompt.length

  return (
    <div className={styles.shell}>
      {/* Breadcrumb */}
      <div className={styles.crumb}>
        <Link className={styles.crumbLink} to={`/ws/${workspaceId}/cases/${caseId}`}>
          <IconChevLeft size={12} />
          {t('caseAgentBack', { id: caseId })}
        </Link>
        <span className={styles.crumbSep}>·</span>
        <span className={['truncate', styles.crumbTitle].join(' ')}>{caseData.title}</span>
        <span style={{ flex: 1 }} />
        {caseData.slackChannelURL && (
          <a
            className={styles.crumbLink}
            href={caseData.slackChannelURL}
            target="_blank"
            rel="noopener noreferrer"
          >
            #{caseData.slackChannelID}
            <IconExt size={11} />
          </a>
        )}
      </div>

      {/* Header */}
      <div className={styles.header}>
        <div className="col" style={{ gap: 4 }}>
          <div className="row" style={{ gap: 10, alignItems: 'center' }}>
            <span className={styles.headerIcon}>
              <IconRobot size={22} sw={1.6} />
            </span>
            <h1 className={styles.headerTitle}>{t('caseAgentTitle')}</h1>
          </div>
          <div className={styles.headerSub}>{t('caseAgentSubtitle')}</div>
        </div>
      </div>

      {updateState.error && (
        <div className={styles.errorBanner}>
          {t('caseAgentSaveError', { message: updateState.error.message })}
        </div>
      )}

      {/* Settings block — operator inputs that shape the next run. */}
      <div className={styles.settingsBlock}>
        {settingsReadOnly && (
          <div className="h-banner warn" data-testid="agent-settings-read-only-notice">
            <IconWarn size={13} sw={2} />
            <span className="h-banner-text">
              {t('caseAgentArchivedReadOnlyNotice', { caseLabel })}
            </span>
          </div>
        )}
        <div className={styles.sectionHead}>
          <span className={styles.sectionHeadTitle}>
            {t('caseAgentSectionSettings')}
          </span>
          <span className={styles.sectionHeadRule} />
        </div>
        {/* 2-column settings cluster */}
        <div className={styles.settings}>
        {/* Additional prompt card */}
        <div className={['card', styles.settingsCard].join(' ')}>
          <div className={styles.settingsHead}>
            <div className={styles.settingsHeadTitle}>{t('caseAgentAdditionalPrompt')}</div>
            <span className={styles.markdownBadge}>{t('caseAgentMarkdownBadge')}</span>
            <span style={{ flex: 1 }} />
            <span className={styles.promptCount}>
              {t('caseAgentPromptCharCount', { count: promptLength, limit: PROMPT_LIMIT })}
            </span>
            {editingPrompt || settingsReadOnly ? null : (
              <Button
                size="sm"
                className={styles.editBtn}
                icon={<IconEdit size={12} />}
                onClick={handleStartPromptEdit}
                data-testid="agent-prompt-edit-button"
              >
                {t('btnEdit')}
              </Button>
            )}
          </div>
          {editingPrompt ? (
            <div className={styles.promptEditor}>
              <textarea
                className={styles.promptTextarea}
                value={draftPrompt}
                onChange={(e) => setDraftPrompt(e.target.value.slice(0, PROMPT_LIMIT))}
                placeholder={t('caseAgentPromptPlaceholder')}
                aria-label={t('caseAgentAdditionalPrompt')}
                data-testid="agent-prompt-textarea"
              />
              <div className={styles.promptEditorActions}>
                <Button size="sm" variant="ghost" onClick={handleCancelPromptEdit}>
                  {t('btnCancel')}
                </Button>
                <Button
                  size="sm"
                  variant="primary"
                  onClick={() => void handleSavePrompt()}
                  disabled={updateState.loading}
                  data-testid="agent-prompt-save-button"
                >
                  {updateState.loading ? t('caseAgentSaving') : t('btnSave')}
                </Button>
              </div>
            </div>
          ) : (
            <div data-testid="agent-prompt-display">
              <MarkdownView source={caseData.agentAdditionalPrompt || ''} />
            </div>
          )}
        </div>

        {/* Sources card */}
        <div className={['card', styles.settingsCard].join(' ')}>
          <div className={styles.settingsHead}>
            <div className={styles.settingsHeadTitle}>{t('caseAgentSources')}</div>
            <span style={{ fontSize: 11.5, color: 'var(--fg-muted)' }}>
              <b>{caseData.agentSources.length}</b>
              {' / '}
              {sources.length}
            </span>
            <span style={{ flex: 1 }} />
            {editingSources || settingsReadOnly ? null : (
              <Button
                size="sm"
                className={styles.editBtn}
                icon={<IconEdit size={12} />}
                onClick={handleStartSourcesEdit}
              >
                {t('btnEdit')}
              </Button>
            )}
          </div>
          {editingSources ? (
            <>
              <div className={styles.sourceSelector}>
                {sources.length === 0 && (
                  <div className={styles.sourcesEmpty}>{t('caseAgentSourcesEmpty')}</div>
                )}
                {sources.map((s) => {
                  const picked = draftSourceIds.includes(s.id)
                  const disabled = !s.enabled
                  return (
                    <button
                      key={s.id}
                      type="button"
                      className={[
                        styles.sourceSelectorRow,
                        disabled ? styles.sourceSelectorRowDisabled : '',
                      ].join(' ')}
                      onClick={() => !disabled && handleToggleSource(s.id)}
                      disabled={disabled}
                      style={{ width: '100%', textAlign: 'left', background: 'transparent', border: 0 }}
                    >
                      <Checkbox checked={picked} />
                      <SourceIcon kind={s.sourceType} size={14} />
                      <div className={styles.sourceRowBody}>
                        <div className={styles.sourceRowHead}>
                          <span className={styles.sourceRowName}>{s.name}</span>
                          <span className={styles.sourceTypeChip}>
                            {SOURCE_TYPE_LABEL[s.sourceType] ?? s.sourceType}
                          </span>
                          {disabled && (
                            <span className={styles.sourceDisabledChip}>
                              {t('caseAgentSourceDisabled')}
                            </span>
                          )}
                        </div>
                        {s.description && (
                          <div className={styles.sourceRowDesc}>{s.description}</div>
                        )}
                      </div>
                    </button>
                  )
                })}
              </div>
              <div className={styles.sourceSelectorActions}>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setDraftSourceIds([])}
                  disabled={draftSourceIds.length === 0}
                >
                  {t('caseAgentSourcesClearAll')}
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setDraftSourceIds(sources.filter((s) => s.enabled).map((s) => s.id))}
                >
                  {t('caseAgentSourcesSelectAll')}
                </Button>
                <span style={{ flex: 1 }} />
                <Button size="sm" variant="ghost" onClick={handleCancelSourcesEdit}>
                  {t('btnCancel')}
                </Button>
                <Button
                  size="sm"
                  variant="primary"
                  onClick={() => void handleSaveSources()}
                  disabled={updateState.loading}
                >
                  {updateState.loading ? t('caseAgentSaving') : t('btnSave')}
                </Button>
              </div>
            </>
          ) : selectedSources.length === 0 ? (
            <div className={styles.sourcesEmpty}>{t('caseAgentSourcesAll')}</div>
          ) : (
            <div className={styles.sourcesList}>
              {selectedSources.map((s) => (
                <div key={s.id} className={styles.sourceRow}>
                  <Checkbox checked />
                  <SourceIcon kind={s.sourceType} size={14} />
                  <div className={styles.sourceRowBody}>
                    <div className={styles.sourceRowHead}>
                      <span className={styles.sourceRowName}>{s.name}</span>
                      <span className={styles.sourceTypeChip}>
                        {SOURCE_TYPE_LABEL[s.sourceType] ?? s.sourceType}
                      </span>
                      {!s.enabled && (
                        <span className={styles.sourceDisabledChip}>
                          {t('caseAgentSourceDisabled')}
                        </span>
                      )}
                    </div>
                    {s.description && (
                      <div className={styles.sourceRowDesc}>{s.description}</div>
                    )}
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
        </div>
      </div>

      {/* Automated Jobs — read-only definitions that fire against this
          case by condition. Sits between Settings (operator inputs) and
          Results (past run outcomes) so the page reads inputs → schedule
          → outcomes top to bottom. */}
      <div className={styles.jobsBlock}>
        <div className={styles.sectionHead}>
          <span className={styles.sectionHeadTitle}>{t('caseAgentSectionJobs')}</span>
          {!jobsError && !(jobsLoading && jobs.length === 0) && (
            <span className={styles.sectionHeadCount}>
              {t('caseAgentJobsCount', { count: jobs.length })}
            </span>
          )}
          <span className={styles.sectionHeadRule} />
        </div>
        {triggerState.error && (
          <div className={styles.errorBanner} data-testid="agent-job-run-error">
            {t('caseAgentJobRunError', { message: triggerState.error.message })}
          </div>
        )}
        <CaseJobList
          jobs={jobs}
          loading={jobsLoading}
          error={!!jobsError}
          onRetry={() => void refetchJobs()}
          onRun={(jobId) => void handleRunJob(jobId)}
          runningJobIds={runningJobIds}
          pendingJobId={pendingJobId}
        />
      </div>

      {/* Results block — the outcome of past runs. Visually separated
          from Settings above so the page reads as two chapters. */}
      <div className={styles.resultsBlock}>
        <div className={styles.sectionHead}>
          <span className={styles.sectionHeadTitle}>
            {t('caseAgentSectionResults')}
          </span>
          <span className={styles.sectionHeadRule} />
        </div>

      {/* Stats hero — values are computed from the loaded page slice
          plus a single derived KPI (count). Cron / token KPIs render
          placeholders until those signals are wired in. */}
      <div className={['card', styles.statsCard].join(' ')}>
        <KpiCell
          label={t('caseAgentStatLogsTotal')}
          value={String(runLogs.length)}
          sub={t('caseAgentStatTodayCount', { count: countToday(runLogs) })}
        />
        <div className={styles.statDivider} />
        <KpiCell
          label={t('caseAgentStatSuccessRate')}
          value={successRate(runLogs)}
          sub={successFailureSummary(runLogs)}
          tone="ok"
        />
        <div className={styles.statDivider} />
        <KpiCell
          label={t('caseAgentStatAverageDuration')}
          value={averageDuration(runLogs)}
          sub={t('caseAgentStatNoData')}
        />
        <div className={styles.statDivider} />
        <KpiCell
          label={t('caseAgentStatTokens24h')}
          value="—"
          sub={t('caseAgentStatNoData')}
        />
        <div className={styles.statDivider} />
        <KpiCell
          label={t('caseAgentStatNextCron')}
          value="—"
          sub={t('caseAgentStatNoData')}
        />
      </div>

      {/* Run logs */}
      <div className="card" style={{ padding: 0, overflow: 'hidden' }}>
        <div className={styles.runHead}>
          <div className={styles.runHeadTitle}>{t('caseAgentRunLogs')}</div>
          <span className={styles.runHeadCount}>
            {t('caseAgentRunLogsCount', { count: runLogs.length })}
          </span>
          <span style={{ flex: 1 }} />
          <MiniSparkline rows={runLogs} />
          <span className="soft" style={{ fontSize: 11 }}>{t('caseAgentRunLogsRecent24h')}</span>
          <Button size="sm" variant="ghost" disabled>
            <span className="soft" style={{ fontSize: 11 }}>{t('caseAgentRunLogsFilter')}</span>
            <IconChevDown size={11} />
          </Button>
        </div>

        <div className={styles.runTableHead}>
          <span style={{ width: 84 }}>{t('caseAgentRunLogsHeaderStatus')}</span>
          <span style={{ width: 140 }}>{t('caseAgentRunLogsHeaderJob')}</span>
          <span style={{ flex: 1 }}>{t('caseAgentRunLogsHeaderStarted')}</span>
          <span style={{ width: 80, textAlign: 'right' }}>
            {t('caseAgentRunLogsHeaderDuration')}
          </span>
          <span style={{ width: 80, textAlign: 'right' }}>
            {t('caseAgentRunLogsHeaderTrigger')}
          </span>
        </div>
        {runLogLoading && runLogs.length === 0 ? (
          <div className={styles.statusCard}>…</div>
        ) : runLogs.length === 0 ? (
          <div className={styles.statusCard}>{t('caseAgentRunLogsEmpty')}</div>
        ) : (
          runLogs.map((r) => {
            const triggerKey = runTriggerLabelKey(r.eventType)
            return (
            <Link
              key={r.runId}
              to={`/ws/${workspaceId}/cases/${caseId}/agent/runs/${r.runId}`}
              className={styles.runRow}
              data-testid="run-log-row"
            >
              <span className={styles.runColStatus}>
                <StageBadge stage={r.stage} size="sm" />
              </span>
              <span className={['truncate', styles.runColJob].join(' ')}>
                {r.jobName}
                {r.strategy === 'PLANEXEC' ? (
                  <span className={styles.strategyChip}>
                    {t('jobStrategyPlanexec')}
                  </span>
                ) : null}
              </span>
              <div className={styles.runColStartedWrap}>
                <div className={['truncate', styles.runColStarted].join(' ')}>
                  {formatStarted(r.startedAt)}
                </div>
                {r.errorMessage && (
                  <div className={['truncate', styles.runColError].join(' ')}>
                    ⚠ {r.errorMessage}
                  </div>
                )}
              </div>
              <span
                className={[
                  styles.runColDuration,
                  r.stage === 'RUNNING' ? styles.runColDurationRunning : '',
                ].join(' ')}
              >
                {formatDuration(r.durationMs, r.stage, t('caseAgentRunDurationRunning'))}
              </span>
              <span className={styles.runColTrigger}>
                <span className={styles.triggerChip} data-testid="run-log-trigger">
                  {triggerKey ? t(triggerKey) : r.eventType || '—'}
                </span>
              </span>
            </Link>
            )
          })
        )}
        <div className={styles.pagination}>
          <span className={styles.paginationLabel}>
            {t('caseAgentPaginationLabel', { shown: runLogs.length, total: runLogs.length })}
          </span>
          <div className={styles.paginationActions}>
            <Button
              size="sm"
              variant="ghost"
              disabled={currentPage === 0}
              onClick={() => {
                setCursorStack((stack) => stack.slice(0, -1))
                setCurrentPage((p) => Math.max(0, p - 1))
              }}
            >
              <IconChevLeft size={11} />
              {t('caseAgentPaginationPrev')}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={!nextCursor}
              onClick={() => {
                if (!nextCursor) return
                setCursorStack((stack) => [...stack, nextCursor])
                setCurrentPage((p) => p + 1)
              }}
            >
              {t('caseAgentPaginationNext')}
              <IconChevRight size={11} />
            </Button>
          </div>
        </div>
      </div>
      </div>
    </div>
  )
}

interface KpiCellProps {
  label: string
  value: string
  sub: string
  tone?: 'ok'
}
function KpiCell({ label, value, sub, tone }: KpiCellProps) {
  return (
    <div className={styles.statKpi}>
      <div className={styles.statLabel}>{label}</div>
      <div className={[styles.statValue, tone === 'ok' ? styles.statValueOk : ''].join(' ')}>{value}</div>
      <div className={styles.statSub}>{sub}</div>
    </div>
  )
}

function countToday(rows: JobRunLogRow[]): number {
  const today = new Date()
  const y = today.getFullYear()
  const m = today.getMonth()
  const d = today.getDate()
  return rows.filter((r) => {
    const t = new Date(r.startedAt)
    return t.getFullYear() === y && t.getMonth() === m && t.getDate() === d
  }).length
}

function successRate(rows: JobRunLogRow[]): string {
  const finished = rows.filter((r) => r.stage !== 'RUNNING')
  if (finished.length === 0) return '—'
  const ok = finished.filter((r) => r.stage === 'SUCCESS').length
  return `${Math.round((ok / finished.length) * 100)}%`
}

function successFailureSummary(rows: JobRunLogRow[]): string {
  const finished = rows.filter((r) => r.stage !== 'RUNNING')
  const ok = finished.filter((r) => r.stage === 'SUCCESS').length
  const fail = finished.filter((r) => r.stage === 'FAILED').length
  return `${ok} / ${ok + fail}`
}

function averageDuration(rows: JobRunLogRow[]): string {
  const durations = rows.map((r) => r.durationMs).filter((n): n is number => n != null && n > 0)
  if (durations.length === 0) return '—'
  const avg = Math.round(durations.reduce((a, b) => a + b, 0) / durations.length)
  if (avg < 1000) return `${avg}ms`
  const s = Math.round(avg / 100) / 10
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  return `${m}m ${Math.round(s - m * 60)}s`
}

// MiniSparkline renders the 24-bar success/fail/running indicator from
// the rightmost edge of the run-log header in the design (06c).
function MiniSparkline({ rows }: { rows: JobRunLogRow[] }) {
  const bars = rows.slice(0, 24)
  while (bars.length < 24) bars.push({ ...placeholderRow(), runId: `pad-${bars.length}` })
  return (
    <div className={styles.miniSpark}>
      {bars.map((r, i) => {
        const color =
          r.stage === 'SUCCESS'
            ? 'var(--ok)'
            : r.stage === 'FAILED'
              ? 'var(--danger)'
              : r.stage === 'RUNNING'
                ? 'var(--info)'
                : 'var(--line-strong)'
        return (
          <span
            key={r.runId + ':' + i}
            className={styles.miniSparkBar}
            style={{ height: 4 + (i % 5) * 3, background: color }}
          />
        )
      })}
    </div>
  )
}

function placeholderRow(): JobRunLogRow {
  return {
    workspaceId: '',
    caseId: 0,
    jobId: '',
    jobName: '',
    strategy: 'SIMPLE',
    runId: '',
    stage: 'SUCCESS',
    startedAt: '',
    endedAt: null,
    durationMs: 0,
    errorMessage: '',
    eventType: '',
  }
}
