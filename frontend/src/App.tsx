import { Navigate, Routes, Route, useParams } from 'react-router'
import Layout from './components/Layout'
import CaseList from './pages/CaseList'
import CaseDetail from './pages/CaseDetail'
import CaseAgent from './pages/CaseAgent'
import JobRunLogDetail from './pages/JobRunLogDetail'
import MemoDetail from './pages/MemoDetail'
import ActionList from './pages/ActionList'
import CaseBoard from './pages/CaseBoard'
import AssistLogList from './pages/AssistLogList'
import SourceList from './pages/SourceList'
import SourceDetail from './pages/SourceDetail'
import ImportNew from './pages/ImportNew'
import ImportDetail from './pages/ImportDetail'
import KnowledgeList from './pages/KnowledgeList'
import KnowledgeDetail from './pages/KnowledgeDetail'
import Home from './pages/Home'
import WorkspaceGuard from './components/WorkspaceGuard'
import { AuthGuard } from './components/auth/auth-guard'

// Legacy /drafts/:id URLs forward to the unified /cases/:id page so old
// links and Slack ephemerals stay valid after the dedicated draft pages
// were retired.
function DraftDetailRedirect() {
  const { id } = useParams<{ id: string }>()
  return <Navigate to={`../cases/${id ?? ''}`} replace />
}

function App() {
  return (
    <AuthGuard>
      <Routes>
        <Route path="/" element={<Home />} />
        <Route path="/ws/:workspaceId" element={<WorkspaceGuard><Layout /></WorkspaceGuard>}>
          <Route index element={<Navigate to="cases" replace />} />
          <Route path="cases" element={<CaseList />} />
          <Route path="cases/:id" element={<CaseDetail />} />
          <Route path="cases/:id/actions/:actionId" element={<CaseDetail />} />
          <Route path="cases/:id/assists" element={<AssistLogList />} />
          <Route path="cases/:id/agent" element={<CaseAgent />} />
          <Route path="cases/:id/agent/runs/:runId" element={<JobRunLogDetail />} />
          <Route path="cases/:id/memos/:memoId" element={<MemoDetail />} />
          <Route path="board" element={<CaseBoard />} />
          <Route path="actions" element={<ActionList />} />
          <Route path="actions/:actionId" element={<ActionList />} />
          <Route path="actions/case/:caseId" element={<ActionList />} />
          <Route path="actions/case/:caseId/:actionId" element={<ActionList />} />
          {/* Drafts live inside the regular Case list/detail pages; the
              Drafts tab in CaseList filters by status and individual draft
              cases open at /cases/:id like any other case. */}
          <Route path="drafts" element={<Navigate to="../cases" replace />} />
          <Route path="drafts/:id" element={<DraftDetailRedirect />} />
          <Route path="sources" element={<SourceList />} />
          <Route path="sources/:id" element={<SourceDetail />} />
          <Route path="knowledge" element={<KnowledgeList />} />
          <Route path="knowledge/:id" element={<KnowledgeDetail />} />
          {/* /imports has no list page by design — sessions are addressable
              only by ID. A bare /imports navigation lands here from the
              breadcrumb Layout renders, so redirect it to the new-import
              entry point instead of showing a blank page. */}
          <Route path="imports" element={<Navigate to="new" replace />} />
          <Route path="imports/new" element={<ImportNew />} />
          <Route path="imports/:importId" element={<ImportDetail />} />
        </Route>
      </Routes>
    </AuthGuard>
  )
}

export default App
