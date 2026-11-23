import { gql } from '@apollo/client'

// GET_CASE_AGENT_SETTINGS pulls every field the CaseAgent page needs in
// a single round-trip: the prompt, the Source allowlist, and just enough
// Case metadata to render the page header.
export const GET_CASE_AGENT_SETTINGS = gql`
  query GetCaseAgentSettings($workspaceId: String!, $caseId: Int!) {
    case(workspaceId: $workspaceId, id: $caseId) {
      id
      workspaceId
      title
      status
      isPrivate
      accessDenied
      archivedAt
      slackChannelID
      slackChannelURL
      agentAdditionalPrompt
      agentSources {
        id
        name
        sourceType
        description
        enabled
      }
    }
    sources(workspaceId: $workspaceId) {
      id
      name
      sourceType
      description
      enabled
    }
  }
`

export const UPDATE_CASE_AGENT_SETTINGS = gql`
  mutation UpdateCaseAgentSettings(
    $workspaceId: String!
    $input: UpdateCaseAgentSettingsInput!
  ) {
    updateCaseAgentSettings(workspaceId: $workspaceId, input: $input) {
      id
      workspaceId
      agentAdditionalPrompt
      agentSources {
        id
        name
        sourceType
        description
        enabled
      }
    }
  }
`

// GET_CASE_LATEST_JOB_RUN fetches just the most-recent run so the Case
// detail sidebar tile can show the "Last run · <stage> · <relative>"
// summary without pulling the full pagination payload.
export const GET_CASE_LATEST_JOB_RUN = gql`
  query GetCaseLatestJobRun($workspaceId: String!, $caseId: Int!) {
    caseJobRunLogs(workspaceId: $workspaceId, caseId: $caseId, first: 1) {
      items {
        runId
        stage
        startedAt
      }
    }
  }
`

export const GET_CASE_JOB_RUN_LOGS = gql`
  query GetCaseJobRunLogs(
    $workspaceId: String!
    $caseId: Int!
    $first: Int
    $after: String
  ) {
    caseJobRunLogs(
      workspaceId: $workspaceId
      caseId: $caseId
      first: $first
      after: $after
    ) {
      items {
        workspaceId
        caseId
        jobId
        jobName
        strategy
        runId
        traceId
        stage
        startedAt
        endedAt
        durationMs
        errorMessage
        eventType
        eventTriggerAt
      }
      nextCursor
    }
  }
`

export const GET_CASE_JOBS = gql`
  query GetCaseJobs($workspaceId: String!, $caseId: Int!) {
    caseJobs(workspaceId: $workspaceId, caseId: $caseId) {
      id
      workspaceId
      name
      description
      strategy
      quiet
      prompt
      trigger {
        caseEvents
        schedule {
          everySeconds
          cron
        }
      }
    }
  }
`

// TRIGGER_CASE_JOB starts one of the Jobs listed by GET_CASE_JOBS against
// this case. It returns as soon as the run is accepted, so the caller polls
// GET_CASE_JOB_RUN_LOGS to watch the run appear and finish.
export const TRIGGER_CASE_JOB = gql`
  mutation TriggerCaseJob($workspaceId: String!, $caseId: Int!, $jobId: String!) {
    triggerCaseJob(workspaceId: $workspaceId, caseId: $caseId, jobId: $jobId)
  }
`

export const GET_JOB_RUN_LOG = gql`
  query GetJobRunLog($workspaceId: String!, $caseId: Int!, $runId: String!) {
    jobRunLog(workspaceId: $workspaceId, caseId: $caseId, runId: $runId) {
      workspaceId
      caseId
      jobId
      jobName
      strategy
      runId
      traceId
      stage
      startedAt
      endedAt
      durationMs
      errorMessage
      systemPrompt
      eventType
      eventTriggerAt
      costUsd
      model
    }
  }
`

export const GET_JOB_RUN_EVENTS = gql`
  query GetJobRunEvents($workspaceId: String!, $caseId: Int!, $runId: String!) {
    jobRunEvents(workspaceId: $workspaceId, caseId: $caseId, runId: $runId) {
      eventId
      runId
      sequence
      occurredAt
      kind
      parentSequence
      phase
      agentLabel
      processId
      payload
    }
  }
`
