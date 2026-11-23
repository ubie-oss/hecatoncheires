import { describe, expect, it, vi, afterEach } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { ApolloLink } from '@apollo/client'
import { MockedProvider, MockLink, type MockedResponse } from '@apollo/client/testing'
import { MemoryRouter, Routes, Route } from 'react-router'

import { I18nProvider } from '../i18n'
import {
  GET_CASE_AGENT_SETTINGS,
  GET_CASE_JOB_RUN_LOGS,
  GET_CASE_JOBS,
  TRIGGER_CASE_JOB,
} from '../graphql/caseAgent'
import { RUN_POLL_INTERVAL_MS, RUN_POLL_MAX_MS } from '../utils/runPolling'
import CaseAgent from './CaseAgent'

// These tests pin the run-log polling lifecycle: when it starts, and — the
// part that is easy to get wrong — when it stops. Polling is observed by
// counting GetCaseJobRunLogs operations as they pass through the link, so the
// assertions do not depend on Apollo internals.

const WS = 'risk'
const CASE_ID = 7

const settingsMock = (archivedAt: string | null = null): MockedResponse => ({
  request: { query: GET_CASE_AGENT_SETTINGS, variables: { workspaceId: WS, caseId: CASE_ID } },
  maxUsageCount: Number.POSITIVE_INFINITY,
  result: {
    data: {
      case: {
        __typename: 'Case',
        id: CASE_ID,
        title: 'Agent target',
        status: 'OPEN',
        isPrivate: false,
        accessDenied: false,
        archivedAt,
        slackChannelID: null,
        slackChannelURL: null,
        agentAdditionalPrompt: '',
        agentSources: [],
      },
      sources: [],
    },
  },
})

const jobsMock = (): MockedResponse => ({
  request: { query: GET_CASE_JOBS, variables: { workspaceId: WS, caseId: CASE_ID } },
  maxUsageCount: Number.POSITIVE_INFINITY,
  result: {
    data: {
      caseJobs: [
        {
          __typename: 'CaseJob',
          id: 'daily_review',
          name: 'Daily review',
          description: 'summarise',
          strategy: 'SIMPLE',
          quiet: false,
          prompt: 'PROMPT BODY',
          trigger: {
            __typename: 'JobTrigger',
            caseEvents: [],
            schedule: { __typename: 'JobSchedule', everySeconds: 86400, cron: null },
          },
        },
      ],
    },
  },
})

const runLogRow = (runId: string, stage: string) => ({
  __typename: 'JobRunLog',
  workspaceId: WS,
  caseId: CASE_ID,
  jobId: 'daily_review',
  jobName: 'Daily review',
  strategy: 'SIMPLE',
  runId,
  traceId: `trace-${runId}`,
  stage,
  startedAt: '2026-06-01T00:00:00.000Z',
  endedAt: stage === 'RUNNING' ? null : '2026-06-01T00:00:05.000Z',
  durationMs: stage === 'RUNNING' ? null : 5000,
  errorMessage: '',
  eventType: 'manual',
  eventTriggerAt: '2026-06-01T00:00:00.000Z',
})

const runLogsMock = (items: ReturnType<typeof runLogRow>[]): MockedResponse => ({
  request: {
    query: GET_CASE_JOB_RUN_LOGS,
    variables: { workspaceId: WS, caseId: CASE_ID, first: 20, after: null },
  },
  maxUsageCount: Number.POSITIVE_INFINITY,
  result: {
    data: {
      caseJobRunLogs: {
        __typename: 'JobRunLogConnection',
        items,
        nextCursor: null,
      },
    },
  },
})

const triggerMock = (): MockedResponse => ({
  request: {
    query: TRIGGER_CASE_JOB,
    variables: { workspaceId: WS, caseId: CASE_ID, jobId: 'daily_review' },
  },
  maxUsageCount: Number.POSITIVE_INFINITY,
  result: { data: { triggerCaseJob: true } },
})

// renderPage wires the page behind a link that counts run-log fetches.
function renderPage(mocks: MockedResponse[]) {
  const counter = { runLogFetches: 0, triggers: 0 }
  const countingLink = new ApolloLink((operation, forward) => {
    if (operation.operationName === 'GetCaseJobRunLogs') counter.runLogFetches++
    if (operation.operationName === 'TriggerCaseJob') counter.triggers++
    return forward(operation)
  })
  const link = ApolloLink.from([countingLink, new MockLink(mocks)])

  render(
    <MemoryRouter initialEntries={[`/ws/${WS}/cases/${CASE_ID}/agent`]}>
      <MockedProvider link={link}>
        <I18nProvider defaultLang="en">
          <Routes>
            <Route path="/ws/:workspaceId/cases/:id/agent" element={<CaseAgent />} />
          </Routes>
        </I18nProvider>
      </MockedProvider>
    </MemoryRouter>,
  )
  return counter
}

afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe('CaseAgent run-log polling', () => {
  it('does not poll when nothing is running and nothing was triggered', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const counter = renderPage([
      settingsMock(),
      jobsMock(),
      runLogsMock([runLogRow('run-old', 'SUCCESS')]),
    ])

    await waitFor(() => expect(screen.getByTestId('job-run-button-daily_review')).toBeInTheDocument())
    const afterLoad = counter.runLogFetches

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_POLL_INTERVAL_MS * 4)
    })
    expect(counter.runLogFetches).toBe(afterLoad)
  })

  it('polls when the page opens while a run is already in flight', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const counter = renderPage([
      settingsMock(),
      jobsMock(),
      runLogsMock([runLogRow('run-live', 'RUNNING')]),
    ])

    await waitFor(() => expect(screen.getByTestId('job-run-button-daily_review')).toBeInTheDocument())
    const afterLoad = counter.runLogFetches

    // A run started elsewhere (Slack, a schedule) must be followed too, even
    // though this page never pressed Run.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_POLL_INTERVAL_MS * 2 + 100)
    })
    expect(counter.runLogFetches).toBeGreaterThan(afterLoad)
  })

  it('stops polling once the in-flight run reaches a terminal stage', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    // The first fetch shows RUNNING; every later fetch shows SUCCESS.
    const counter = renderPage([
      settingsMock(),
      jobsMock(),
      { ...runLogsMock([runLogRow('run-live', 'RUNNING')]), maxUsageCount: 1 },
      runLogsMock([runLogRow('run-live', 'SUCCESS')]),
    ])

    await waitFor(() => expect(screen.getByTestId('job-run-button-daily_review')).toBeInTheDocument())
    const afterLoad = counter.runLogFetches

    // The poll that flips the row to SUCCESS must actually happen…
    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_POLL_INTERVAL_MS + 100)
    })
    const afterTerminal = counter.runLogFetches
    expect(afterTerminal).toBeGreaterThan(afterLoad)

    // …and then polling must stop, without waiting out an appearance window.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_POLL_INTERVAL_MS * 4)
    })
    expect(counter.runLogFetches).toBe(afterTerminal)
  })

  it('gives up polling when a triggered run never shows a RUNNING row', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    // The run log never changes: the run finished between two polls, so its
    // RUNNING row is never observed. Polling must still end.
    const counter = renderPage([
      settingsMock(),
      jobsMock(),
      runLogsMock([runLogRow('run-old', 'SUCCESS')]),
      triggerMock(),
    ])

    const runButton = await screen.findByTestId('job-run-button-daily_review')
    fireEvent.click(runButton)
    // Let the mutation settle so the page has registered the appearance
    // window before any poll interval elapses.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10)
    })
    expect(counter.triggers).toBe(1)
    const afterTrigger = counter.runLogFetches

    // Polling is live during the appearance window.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_POLL_INTERVAL_MS * 2)
    })
    expect(counter.runLogFetches).toBeGreaterThan(afterTrigger)

    // Past the window it must stop, even though no fetch ever returned new
    // data (an unchanged poll result does not re-render the page).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_POLL_MAX_MS)
    })
    const afterDeadline = counter.runLogFetches
    await act(async () => {
      await vi.advanceTimersByTimeAsync(RUN_POLL_INTERVAL_MS * 4)
    })
    expect(counter.runLogFetches).toBe(afterDeadline)
  })
})

describe('CaseAgent settings on an archived case', () => {
  it('offers the prompt Edit button on an active case', async () => {
    renderPage([settingsMock(), jobsMock(), runLogsMock([])])

    expect(await screen.findByTestId('agent-prompt-edit-button')).toBeInTheDocument()
    expect(screen.queryByTestId('agent-settings-read-only-notice')).toBeNull()
  })

  it('hides both Edit buttons and says why on an archived case', async () => {
    renderPage([settingsMock('2026-06-02T00:00:00Z'), jobsMock(), runLogsMock([])])

    expect(await screen.findByTestId('agent-settings-read-only-notice')).toHaveTextContent(
      'agent settings cannot be edited',
    )
    expect(screen.queryByTestId('agent-prompt-edit-button')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Edit' })).toBeNull()
    // The current settings are still shown.
    expect(screen.getByTestId('agent-prompt-display')).toBeInTheDocument()
  })
})
