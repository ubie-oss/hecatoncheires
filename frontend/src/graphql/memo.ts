import { gql } from '@apollo/client'

const MEMO_FIELDS = `
  id
  caseID
  title
  fields {
    fieldId
    value
    display {
      label
      url
    }
  }
  archivedAt
  createdAt
  updatedAt
`

export const GET_MEMOS_BY_CASE = gql`
  query GetMemosByCase($workspaceId: String!, $caseID: Int!, $filter: MemoArchiveFilter) {
    memosByCase(workspaceId: $workspaceId, caseID: $caseID, filter: $filter) {
      ${MEMO_FIELDS}
    }
  }
`

export const GET_MEMO = gql`
  query GetMemo($workspaceId: String!, $caseID: Int!, $id: ID!) {
    memo(workspaceId: $workspaceId, caseID: $caseID, id: $id) {
      ${MEMO_FIELDS}
      case {
        id
        workspaceId
        archivedAt
      }
    }
  }
`

export const GET_MEMO_CONFIGURATION = gql`
  query GetMemoConfiguration($workspaceId: String!) {
    memoConfiguration(workspaceId: $workspaceId) {
      description
      fields {
        id
        name
        type
        required
        description
        options {
          id
          name
          description
          metadata
        }
      }
    }
  }
`

export const CREATE_MEMO = gql`
  mutation CreateMemo($workspaceId: String!, $input: CreateMemoInput!) {
    createMemo(workspaceId: $workspaceId, input: $input) {
      ${MEMO_FIELDS}
    }
  }
`

export const UPDATE_MEMO = gql`
  mutation UpdateMemo($workspaceId: String!, $input: UpdateMemoInput!) {
    updateMemo(workspaceId: $workspaceId, input: $input) {
      ${MEMO_FIELDS}
    }
  }
`

export const ARCHIVE_MEMO = gql`
  mutation ArchiveMemo($workspaceId: String!, $caseID: Int!, $id: ID!) {
    archiveMemo(workspaceId: $workspaceId, caseID: $caseID, id: $id) {
      ${MEMO_FIELDS}
    }
  }
`

export const UNARCHIVE_MEMO = gql`
  mutation UnarchiveMemo($workspaceId: String!, $caseID: Int!, $id: ID!) {
    unarchiveMemo(workspaceId: $workspaceId, caseID: $caseID, id: $id) {
      ${MEMO_FIELDS}
    }
  }
`
