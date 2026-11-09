import { gql } from '@apollo/client'

const ACTION_FIELDS = `
  id
  workspaceId
  caseID
  case {
    id
    workspaceId
    title
    accessDenied
    fields {
      fieldId
      value
    }
    slackChannelID
    slackChannelURL
    slackThreadTS
  }
  title
  description
  assigneeID
  assignee {
    id
    name
    realName
    imageUrl
  }
  slackMessageTS
  status
  dueDate
  archived
  archivedAt
  createdAt
  updatedAt
  stepProgress {
    done
    total
  }
`

export const GET_ACTIONS = gql`
  query GetActions($workspaceId: String!) {
    actions(workspaceId: $workspaceId) {
      ${ACTION_FIELDS}
    }
  }
`

export const GET_ACTION = gql`
  query GetAction($workspaceId: String!, $id: Int!) {
    action(workspaceId: $workspaceId, id: $id) {
      ${ACTION_FIELDS}
    }
  }
`

export const GET_ACTION_MESSAGES = gql`
  query GetActionMessages($workspaceId: String!, $id: Int!, $limit: Int, $cursor: String) {
    action(workspaceId: $workspaceId, id: $id) {
      id
      workspaceId
      messages(limit: $limit, cursor: $cursor) {
        items {
          id
          channelID
          threadTS
          teamID
          userID
          userName
          text
          createdAt
          files {
            id
            name
            mimetype
            filetype
            size
            urlPrivate
            permalink
            thumbURL
          }
        }
        nextCursor
      }
    }
  }
`

export const GET_ACTION_EVENTS = gql`
  query GetActionEvents($workspaceId: String!, $id: Int!, $limit: Int, $cursor: String) {
    action(workspaceId: $workspaceId, id: $id) {
      id
      workspaceId
      events(limit: $limit, cursor: $cursor) {
        items {
          id
          actionID
          kind
          actorID
          actor {
            id
            name
            realName
            imageUrl
          }
          oldValue
          newValue
          createdAt
        }
        nextCursor
      }
    }
  }
`

export const CREATE_ACTION = gql`
  mutation CreateAction($workspaceId: String!, $input: CreateActionInput!) {
    createAction(workspaceId: $workspaceId, input: $input) {
      ${ACTION_FIELDS}
    }
  }
`

export const UPDATE_ACTION = gql`
  mutation UpdateAction($workspaceId: String!, $input: UpdateActionInput!) {
    updateAction(workspaceId: $workspaceId, input: $input) {
      ${ACTION_FIELDS}
    }
  }
`

export const ARCHIVE_ACTION = gql`
  mutation ArchiveAction($workspaceId: String!, $id: Int!) {
    archiveAction(workspaceId: $workspaceId, id: $id) {
      ${ACTION_FIELDS}
    }
  }
`

export const UNARCHIVE_ACTION = gql`
  mutation UnarchiveAction($workspaceId: String!, $id: Int!) {
    unarchiveAction(workspaceId: $workspaceId, id: $id) {
      ${ACTION_FIELDS}
    }
  }
`

// Archiving runs asynchronously on the server; the mutation returns the
// accepted action ids immediately (a scalar Int list, no subfields).
export const BULK_ARCHIVE_ACTIONS = gql`
  mutation BulkArchiveActions($workspaceId: String!, $ids: [Int!]!) {
    bulkArchiveActions(workspaceId: $workspaceId, ids: $ids)
  }
`

export const POST_ACTION_SLACK_MESSAGE = gql`
  mutation PostActionSlackMessage($workspaceId: String!, $id: Int!) {
    postActionSlackMessage(workspaceId: $workspaceId, id: $id) {
      ${ACTION_FIELDS}
    }
  }
`

export const GET_OPEN_CASE_ACTIONS = gql`
  query GetOpenCaseActions($workspaceId: String!) {
    openCaseActions(workspaceId: $workspaceId) {
      ${ACTION_FIELDS}
    }
  }
`

export const GET_ACTIONS_BY_CASE = gql`
  query GetActionsByCase($workspaceId: String!, $caseID: Int!) {
    actionsByCase(workspaceId: $workspaceId, caseID: $caseID) {
      ${ACTION_FIELDS}
    }
  }
`

const ACTION_COMMENT_FIELDS = `
  id
  actionID
  authorID
  body
  createdAt
  updatedAt
  edited
  author {
    id
    name
    realName
    imageUrl
  }
`

export const GET_ACTION_COMMENTS = gql`
  query GetActionComments($workspaceId: String!, $id: Int!, $limit: Int, $cursor: String) {
    action(workspaceId: $workspaceId, id: $id) {
      id
      workspaceId
      comments(limit: $limit, cursor: $cursor) {
        items {
          ${ACTION_COMMENT_FIELDS}
        }
        nextCursor
      }
    }
  }
`

export const CREATE_ACTION_COMMENT = gql`
  mutation CreateActionComment($workspaceId: String!, $input: CreateActionCommentInput!) {
    createActionComment(workspaceId: $workspaceId, input: $input) {
      ${ACTION_COMMENT_FIELDS}
    }
  }
`

export const UPDATE_ACTION_COMMENT = gql`
  mutation UpdateActionComment($workspaceId: String!, $input: UpdateActionCommentInput!) {
    updateActionComment(workspaceId: $workspaceId, input: $input) {
      ${ACTION_COMMENT_FIELDS}
    }
  }
`

export const DELETE_ACTION_COMMENT = gql`
  mutation DeleteActionComment($workspaceId: String!, $input: DeleteActionCommentInput!) {
    deleteActionComment(workspaceId: $workspaceId, input: $input)
  }
`
