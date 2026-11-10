import { NavLink, Link } from 'react-router'
import { useQuery } from '@apollo/client'
import { useWorkspace } from '../contexts/workspace-context'
import { useAuth } from '../contexts/auth-context'
import { useTranslation } from '../i18n'
import { GET_CASES } from '../graphql/case'
import { GET_ACTIONS } from '../graphql/action'
import { GET_SOURCES } from '../graphql/source'
import { GET_KNOWLEDGES } from '../graphql/knowledge'
import { useCaseStatuses } from '../hooks/useCaseStatuses'
import { useActionsEnabled } from '../hooks/useActionsEnabled'
import { Avatar } from './Primitives'
import type { ComponentType } from 'react'
import {
  IconCases,
  IconActions,
  IconBoard,
  IconSources,
  IconKnowledge,
  IconSettings,
  IconUser,
} from './Icons'

interface NavItem {
  id: string
  label: string
  Icon: ComponentType<{ size?: number }>
  to: string
  count: number | null
}

interface NavCount {
  cases: number | null
  actions: number | null
  sources: number | null
  knowledge: number | null
}

function useNavCounts(workspaceId: string | undefined): NavCount {
  const { data: cases } = useQuery(GET_CASES, {
    variables: { workspaceId, status: 'OPEN' },
    skip: !workspaceId,
  })
  const { data: actions } = useQuery(GET_ACTIONS, {
    variables: { workspaceId },
    skip: !workspaceId,
  })
  const { data: sources } = useQuery(GET_SOURCES, {
    variables: { workspaceId },
    skip: !workspaceId,
  })
  const { data: knowledges } = useQuery(GET_KNOWLEDGES, {
    variables: { workspaceId },
    skip: !workspaceId,
  })
  return {
    cases: cases?.cases?.length ?? null,
    actions: actions?.actions?.length ?? null,
    sources: sources?.sources?.length ?? null,
    knowledge: knowledges?.knowledges?.length ?? null,
  }
}

export default function Sidebar() {
  const { currentWorkspace } = useWorkspace()
  const { user } = useAuth()
  const { t } = useTranslation()
  const wsPrefix = currentWorkspace ? `/ws/${currentWorkspace.id}` : ''
  const counts = useNavCounts(currentWorkspace?.id)
  // A thread-mode workspace (one with a Case status set) gets the Case board.
  // Actions are listed only where the workspace's Cases have them: always in
  // channel mode, and in thread mode when [case] actions is on. Until the
  // answer arrives, fall back to the mode so a channel-mode workspace does not
  // flash without its Actions entry.
  const { isThreadMode } = useCaseStatuses(currentWorkspace?.id)
  const actionsEnabled = useActionsEnabled(currentWorkspace?.id) ?? !isThreadMode

  const items: NavItem[] = [
    { id: 'cases',      label: t('navCases'),      Icon: IconCases,     to: `${wsPrefix}/cases`,      count: counts.cases },
    ...(isThreadMode
      ? [{ id: 'board', label: t('navBoard'), Icon: IconBoard, to: `${wsPrefix}/board`, count: counts.cases }]
      : []),
    ...(actionsEnabled
      ? [{ id: 'actions', label: t('navActions'), Icon: IconActions, to: `${wsPrefix}/actions`, count: counts.actions }]
      : []),
    { id: 'sources',    label: t('navSources'),    Icon: IconSources,   to: `${wsPrefix}/sources`,    count: counts.sources },
    { id: 'knowledge',  label: t('navKnowledge'),  Icon: IconKnowledge, to: `${wsPrefix}/knowledge`,  count: counts.knowledge },
  ]

  return (
    <aside className="h-side">
      <Link to="/" className="h-side-logo" style={{ textDecoration: 'none' }}>
        <img src="/logo-three-heads.png" alt={t('appName')} />
        <span>{t('appName')}</span>
      </Link>
      <nav className="h-side-nav">
        <div className="h-side-section">{t('sidebarSectionWorkspace')}</div>
        {items.map((it) => (
          <NavLink
            key={it.id}
            to={it.to}
            className={({ isActive }) => (isActive ? 'active' : '')}
            end={it.id === 'cases'}
          >
            <span className="nav-icon"><it.Icon size={15} /></span>
            <span>{it.label}</span>
            {it.count != null && <span className="nav-count">{it.count}</span>}
          </NavLink>
        ))}
        <div className="h-side-section">{t('sidebarSectionWorkspaceSettings')}</div>
        <a
          href="#"
          aria-disabled="true"
          title="Coming soon"
          onClick={(e) => e.preventDefault()}
          style={{ opacity: 0.45, cursor: 'not-allowed' }}
        >
          <span className="nav-icon"><IconSettings size={15} /></span>
          <span>{t('navCustomFields')}</span>
        </a>
        <a
          href="#"
          aria-disabled="true"
          title="Coming soon"
          onClick={(e) => e.preventDefault()}
          style={{ opacity: 0.45, cursor: 'not-allowed' }}
        >
          <span className="nav-icon"><IconUser size={15} /></span>
          <span>{t('navMembers')}</span>
        </a>
      </nav>
      {user && (
        <div className="h-side-foot">
          <Avatar size="sm" name={user.name} />
          <div className="col" style={{ gap: 0, lineHeight: 1.2, minWidth: 0 }}>
            <div style={{ fontSize: 12, fontWeight: 500 }} className="truncate">{user.name}</div>
            <div style={{ fontSize: 11, color: 'var(--side-fg-muted)' }} className="truncate">{user.email}</div>
          </div>
        </div>
      )}
    </aside>
  )
}
