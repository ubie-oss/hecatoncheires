import { Navigate, useLocation } from 'react-router'
import { useWorkspace } from '../contexts/workspace-context'
import { useCaseStatuses } from '../hooks/useCaseStatuses'
import CaseKanban from './CaseKanban'

// CaseBoard is the /board route: the Case Kanban of a thread-mode workspace.
// A channel-mode workspace has no Case status set to lay out as columns, so
// the route forwards to its Action board instead.
export default function CaseBoard() {
  const { search } = useLocation()
  const { currentWorkspace } = useWorkspace()
  const { isThreadMode, loading } = useCaseStatuses(currentWorkspace?.id)

  if (loading) return null
  if (!isThreadMode) {
    return <Navigate to={`/ws/${currentWorkspace?.id ?? ''}/actions${search}`} replace />
  }
  return <CaseKanban />
}
