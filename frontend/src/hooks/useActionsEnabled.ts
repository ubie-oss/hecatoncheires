import { useQuery } from '@apollo/client'
import { GET_ACTIONS_ENABLED } from '../graphql/action'

// useActionsEnabled reports whether the workspace's Cases have Actions: always
// true for a channel-mode workspace, and for a thread-mode workspace only when
// its [case] actions option is on. It is undefined until the answer arrives
// (or when the query fails), so callers pick their own fallback rather than
// treating "unknown" as "disabled".
export function useActionsEnabled(workspaceId: string | undefined): boolean | undefined {
  const { data } = useQuery(GET_ACTIONS_ENABLED, {
    variables: { workspaceId: workspaceId ?? '' },
    skip: !workspaceId,
  })
  const enabled = data?.actionsEnabled
  return typeof enabled === 'boolean' ? enabled : undefined
}
