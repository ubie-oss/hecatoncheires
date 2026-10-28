import { gql } from '@apollo/client'

// Shared selection sets. The fragments form a linear chain
// (CaseListFields ⊃ CaseMutationFields ⊃ CaseUserFields) and each
// operation interpolates exactly one root fragment constant, so a
// document never contains duplicate fragment definitions.

const CASE_USER_FIELDS = gql`
  fragment CaseUserFields on SlackUser {
    id
    name
    realName
    imageUrl
  }
`

// The selection set returned by case mutations (create / update /
// close / reopen). Deliberately narrower than CaseListFields: the
// mutation responses historically never carried slackThreadTS /
// isThreadBound / boardStatus, and this refactor keeps every
// operation's field set byte-equivalent to what it was.
const CASE_MUTATION_FIELDS = gql`
  ${CASE_USER_FIELDS}
  fragment CaseMutationFields on Case {
    id
    workspaceId
    title
    description
    status
    # archivedAt is null for an active case. There is no derived archived
    # boolean on the wire; the UI checks archivedAt != null. It rides in the
    # mutation fragment (not only the list one) so archiveCase / unarchiveCase
    # refresh the detail page's badge and menu from their own response.
    archivedAt
    isPrivate
    isTest
    accessDenied
    reporterID
    reporter {
      ...CaseUserFields
    }
    assigneeIDs
    assignees {
      ...CaseUserFields
    }
    slackChannelID
    createdAt
    updatedAt
    fields {
      fieldId
      value
      display {
        label
        url
      }
    }
  }
`

const CASE_LIST_FIELDS = gql`
  ${CASE_MUTATION_FIELDS}
  fragment CaseListFields on Case {
    ...CaseMutationFields
    slackThreadTS
    isThreadBound
    boardStatus
  }
`

export const GET_CASES = gql`
  ${CASE_LIST_FIELDS}
  query GetCases($workspaceId: String!, $status: CaseStatus, $filter: CaseArchiveFilter) {
    cases(workspaceId: $workspaceId, status: $status, filter: $filter) {
      ...CaseListFields
    }
  }
`

// GET_CASES_WITH_SLACK_LINK is the Case list page's own operation: GET_CASES
// plus slackChannelURL, which it needs to build a workspace-qualified Slack
// link per row.
//
// That field is deliberately NOT in CaseListFields. Its resolver calls Slack's
// auth.test, and a failure there is cached for the life of the server process,
// so every consumer of the shared GET_CASES — the Case board, the sidebar
// counts, the Action form's case picker, and the refetches that follow a
// mutation — would break for good on a misconfigured Slack. None of them need
// the link, so only this operation carries the risk (and the Case list pairs
// it with errorPolicy 'all' so its own rows survive).
export const GET_CASES_WITH_SLACK_LINK = gql`
  ${CASE_LIST_FIELDS}
  query GetCasesWithSlackLink($workspaceId: String!, $status: CaseStatus, $filter: CaseArchiveFilter) {
    cases(workspaceId: $workspaceId, status: $status, filter: $filter) {
      ...CaseListFields
      slackChannelURL
    }
  }
`

export const GET_CASE = gql`
  ${CASE_LIST_FIELDS}
  query GetCase($workspaceId: String!, $id: Int!, $actionsFilter: ActionArchiveFilter) {
    case(workspaceId: $workspaceId, id: $id) {
      ...CaseListFields
      channelUserCount
      slackChannelURL
      actions(filter: $actionsFilter) {
        id
        workspaceId
        title
        status
        assigneeID
        assignee {
          ...CaseUserFields
        }
        dueDate
        archived
        archivedAt
        createdAt
        updatedAt
      }
    }
  }
`

export const GET_CASE_MEMBERS = gql`
  ${CASE_USER_FIELDS}
  query GetCaseMembers($workspaceId: String!, $id: Int!, $limit: Int, $offset: Int, $filter: String) {
    case(workspaceId: $workspaceId, id: $id) {
      id
      workspaceId
      channelUserCount
      channelUsers(limit: $limit, offset: $offset, filter: $filter) {
        items {
          ...CaseUserFields
        }
        totalCount
        hasMore
      }
    }
  }
`

export const CREATE_CASE = gql`
  ${CASE_MUTATION_FIELDS}
  mutation CreateCase($workspaceId: String!, $input: CreateCaseInput!) {
    createCase(workspaceId: $workspaceId, input: $input) {
      ...CaseMutationFields
    }
  }
`

// Searches the channels the acting user may link a new case to. The server
// keeps the user's channel list and searches it; refresh: true makes it fetch
// the list from Slack first.
export const GET_EXISTING_SLACK_CHANNEL_CANDIDATES = gql`
  query GetExistingSlackChannelCandidates($workspaceId: String!, $query: String, $refresh: Boolean) {
    existingSlackChannelCandidates(workspaceId: $workspaceId, query: $query, refresh: $refresh) {
      channels {
        id
        name
        isPrivate
      }
      hasMore
      fetchedAt
    }
  }
`

export const UPDATE_CASE = gql`
  ${CASE_MUTATION_FIELDS}
  mutation UpdateCase($workspaceId: String!, $input: UpdateCaseInput!) {
    updateCase(workspaceId: $workspaceId, input: $input) {
      ...CaseMutationFields
    }
  }
`

export const DELETE_CASE = gql`
  mutation DeleteCase($workspaceId: String!, $id: Int!) {
    deleteCase(workspaceId: $workspaceId, id: $id)
  }
`

export const CLOSE_CASE = gql`
  ${CASE_MUTATION_FIELDS}
  mutation CloseCase($workspaceId: String!, $id: Int!) {
    closeCase(workspaceId: $workspaceId, id: $id) {
      ...CaseMutationFields
    }
  }
`

export const REOPEN_CASE = gql`
  ${CASE_MUTATION_FIELDS}
  mutation ReopenCase($workspaceId: String!, $id: Int!) {
    reopenCase(workspaceId: $workspaceId, id: $id) {
      ...CaseMutationFields
    }
  }
`

// ARCHIVE_CASE / UNARCHIVE_CASE act on one case and run synchronously, so the
// response is the authoritative post-change Case and the caller can render it
// directly.
export const ARCHIVE_CASE = gql`
  ${CASE_MUTATION_FIELDS}
  mutation ArchiveCase($workspaceId: String!, $id: Int!) {
    archiveCase(workspaceId: $workspaceId, id: $id) {
      ...CaseMutationFields
    }
  }
`

export const UNARCHIVE_CASE = gql`
  ${CASE_MUTATION_FIELDS}
  mutation UnarchiveCase($workspaceId: String!, $id: Int!) {
    unarchiveCase(workspaceId: $workspaceId, id: $id) {
      ...CaseMutationFields
    }
  }
`

// BULK_ARCHIVE_CASES / BULK_UNARCHIVE_CASES return the ids ACCEPTED for the
// change, not the ids already changed: the server processes them
// asynchronously so the call survives the request being cancelled. A refetch
// straight after would therefore not reflect completion — the caller removes
// the accepted rows locally instead.
export const BULK_ARCHIVE_CASES = gql`
  mutation BulkArchiveCases($workspaceId: String!, $ids: [Int!]!) {
    bulkArchiveCases(workspaceId: $workspaceId, ids: $ids)
  }
`

export const BULK_UNARCHIVE_CASES = gql`
  mutation BulkUnarchiveCases($workspaceId: String!, $ids: [Int!]!) {
    bulkUnarchiveCases(workspaceId: $workspaceId, ids: $ids)
  }
`

export const SYNC_CASE_CHANNEL_USERS = gql`
  mutation SyncCaseChannelUsers($workspaceId: String!, $id: Int!) {
    syncCaseChannelUsers(workspaceId: $workspaceId, id: $id) {
      id
      workspaceId
      channelUserCount
    }
  }
`

// ASSIGN_CASE / UNASSIGN_CASE mutate the assignee set by delta (add / remove
// the listed users) instead of replacing the whole list via updateCase. The
// server applies the change atomically, so concurrent edits cannot clobber
// one another. Assignees can ONLY be changed through these — updateCase /
// submitDraft no longer accept assigneeIDs.
export const ASSIGN_CASE = gql`
  ${CASE_USER_FIELDS}
  mutation AssignCase($workspaceId: String!, $id: Int!, $userIDs: [String!]!) {
    assignCase(workspaceId: $workspaceId, id: $id, userIDs: $userIDs) {
      id
      workspaceId
      assigneeIDs
      assignees {
        ...CaseUserFields
      }
      updatedAt
    }
  }
`

export const UNASSIGN_CASE = gql`
  ${CASE_USER_FIELDS}
  mutation UnassignCase($workspaceId: String!, $id: Int!, $userIDs: [String!]!) {
    unassignCase(workspaceId: $workspaceId, id: $id, userIDs: $userIDs) {
      id
      workspaceId
      assigneeIDs
      assignees {
        ...CaseUserFields
      }
      updatedAt
    }
  }
`
