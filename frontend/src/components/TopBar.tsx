import { useLocation, useParams, useNavigate } from 'react-router'
import { useWorkspace } from '../contexts/workspace-context'
import { useTranslation } from '../i18n'
import WorkspaceSwitcher from './WorkspaceSwitcher'
import { UserMenu } from './UserMenu'
import { IconBell, IconSearch } from './Icons'
import type { ReactNode } from 'react'

interface Crumb {
  label: string
  to?: string
}

function useBreadcrumbs(): Crumb[] {
  const { pathname } = useLocation()
  const { workspaceId } = useParams<{ workspaceId: string }>()
  const { t } = useTranslation()
  if (!workspaceId) return []

  const wsPath = `/ws/${workspaceId}`
  const parts = pathname.replace(wsPath, '').split('/').filter(Boolean)
  if (parts.length === 0) return [{ label: t('navCases') }]

  const top = parts[0]
  const sectionLabel: Record<string, string> = {
    cases: t('navCases'),
    board: t('navBoard'),
    actions: t('navActions'),
    sources: t('navSources'),
  }

  const crumbs: Crumb[] = [{ label: sectionLabel[top] || top, to: `${wsPath}/${top}` }]
  if (parts.length >= 2) crumbs.push({ label: `#${parts[1]}` })
  return crumbs
}

export default function TopBar() {
  const navigate = useNavigate()
  const { workspaceId } = useParams<{ workspaceId: string }>()
  const { currentWorkspace, workspaces, favoriteWorkspaceIds } = useWorkspace()
  const { t } = useTranslation()
  const crumbs = useBreadcrumbs()

  const renderedCrumbs: ReactNode[] = []
  crumbs.forEach((c, i) => {
    if (i > 0) {
      renderedCrumbs.push(
        <span key={`sep-${i}`} className="sep">/</span>
      )
    }
    renderedCrumbs.push(
      c.to ? (
        <a
          key={`c-${i}`}
          href={c.to}
          className="crumb"
          style={{ color: i === crumbs.length - 1 ? 'var(--fg)' : undefined, textDecoration: 'none' }}
          onClick={(e) => { e.preventDefault(); if (c.to) navigate(c.to) }}
        >
          {c.label}
        </a>
      ) : (
        <span
          key={`c-${i}`}
          style={{ color: i === crumbs.length - 1 ? 'var(--fg)' : undefined }}
        >
          {c.label}
        </span>
      )
    )
  })

  return (
    <header className="h-top">
      {workspaceId && (
        <WorkspaceSwitcher
          current={currentWorkspace}
          workspaces={workspaces}
          favoriteWorkspaceIds={favoriteWorkspaceIds}
        />
      )}
      {renderedCrumbs.length > 0 && (
        <div className="h-bread">
          <span className="sep">/</span>
          {renderedCrumbs}
        </div>
      )}
      <div
        className="h-search"
        aria-disabled="true"
        title="Coming soon"
        style={{ opacity: 0.6, cursor: 'not-allowed' }}
      >
        <IconSearch size={14} />
        <span>{t('topbarSearchPlaceholder')}</span>
        <span style={{
          marginLeft: 'auto', fontSize: 10, padding: '1px 5px', borderRadius: 3,
          background: 'var(--bg-sunken)', border: '1px solid var(--line)',
        }}>⌘K</span>
      </div>
      <button
        className="h-icon-btn"
        title="Coming soon"
        aria-label="Notifications"
        aria-disabled="true"
        disabled
        style={{ opacity: 0.5, cursor: 'not-allowed' }}
      >
        <IconBell size={16} />
      </button>
      <UserMenu />
    </header>
  )
}
