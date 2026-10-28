import { gql } from '@apollo/client'

export const GET_FIELD_CONFIGURATION = gql`
  query GetFieldConfiguration($workspaceId: String!) {
    fieldConfiguration(workspaceId: $workspaceId) {
      fields {
        id
        name
        type
        required
        description
        referenceWorkspaceId
        options {
          id
          name
          description
          metadata
        }
      }
      labels {
        case
      }
      actionConfig {
        initial
        closed
        statuses {
          id
          name
          description
          color
          emoji
        }
      }
      allowExistingSlackChannel
    }
  }
`
