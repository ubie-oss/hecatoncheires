import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery } from '@apollo/client'
import {
  ADD_ACTION_STEP,
  DELETE_ACTION_STEP,
  GET_ACTION_STEPS,
  RENAME_ACTION_STEP,
  SET_ACTION_STEP_DONE,
} from '../graphql/actionStep'
import { useTranslation } from '../i18n'
import { commitOnEnter } from '../utils/keyboard'
import styles from './StepList.module.css'

interface Step {
  id: string
  actionID: number
  title: string
  done: boolean
  doneAt?: string | null
  createdAt: string
  updatedAt: string
}

interface Props {
  workspaceId: string
  actionId: number
  /** Render the steps without any control that changes them (the Action or
   *  its Case is archived, and the server rejects step writes). */
  readOnly?: boolean
}

// 1:1 visual port of the design source under tmp/hecatoncheires
// (screens-2.jsx). Layout, sizes, tokens, and progress-bar treatment are
// reproduced directly; the metadata that the WebUI deliberately hides
// (createdBy / doneBy / timestamps) stays out of the row even though
// GraphQL can deliver it.
export default function StepList({ workspaceId, actionId, readOnly = false }: Props) {
  const { t } = useTranslation()
  const refetchVars = { workspaceId, id: actionId }

  const { data } = useQuery(GET_ACTION_STEPS, { variables: refetchVars })

  const steps: Step[] = data?.action?.steps ?? []
  const progress = data?.action?.stepProgress ?? { done: 0, total: 0 }

  const refetchOpts = {
    refetchQueries: [{ query: GET_ACTION_STEPS, variables: refetchVars }],
  }

  const [addStep] = useMutation(ADD_ACTION_STEP, refetchOpts)
  const [setDone] = useMutation(SET_ACTION_STEP_DONE, refetchOpts)
  const [renameStep] = useMutation(RENAME_ACTION_STEP, refetchOpts)
  const [deleteStep] = useMutation(DELETE_ACTION_STEP, refetchOpts)

  const handleToggle = async (step: Step) => {
    await setDone({
      variables: { workspaceId, input: { actionId, stepId: step.id, done: !step.done } },
    })
  }

  const handleRename = async (step: Step, nextTitle: string) => {
    const title = nextTitle.trim()
    if (title === '' || title === step.title) return
    await renameStep({
      variables: { workspaceId, input: { actionId, stepId: step.id, title } },
    })
  }

  const handleDelete = async (step: Step) => {
    await deleteStep({
      variables: { workspaceId, input: { actionId, stepId: step.id } },
    })
  }

  const handleAdd = async (title: string) => {
    const trimmed = title.trim()
    if (trimmed === '') return
    await addStep({
      variables: { workspaceId, input: { actionId, title: trimmed } },
    })
  }

  const progressRatio = progress.total > 0 ? progress.done / progress.total : 0

  return (
    <div className={styles.section} data-testid="action-step-list">
      <div className={styles.header}>
        <span className={styles.headerLabel}>{t('sectionActionSteps')}</span>
        {progress.total > 0 && (
          <span className={styles.progressPill} data-testid="action-step-progress">
            {progress.done}/{progress.total}
          </span>
        )}
      </div>

      {progress.total > 0 && (
        <div className={styles.progressTrack} aria-hidden>
          <div
            className={styles.progressFill}
            style={{ width: `${progressRatio * 100}%` }}
          />
        </div>
      )}

      <div className={styles.list}>
        {steps.map((step) => (
          <StepRow
            key={step.id}
            step={step}
            onToggle={() => handleToggle(step)}
            onRename={(next) => handleRename(step, next)}
            onDelete={() => handleDelete(step)}
            ariaCheckboxLabel={t('ariaActionStepCheckbox')}
            ariaDeleteLabel={t('ariaDeleteActionStep')}
            readOnly={readOnly}
          />
        ))}
        {!readOnly && (
          <AddStepRow
            placeholder={t('placeholderAddActionStep')}
            onSubmit={handleAdd}
          />
        )}
      </div>
    </div>
  )
}

interface StepRowProps {
  step: Step
  onToggle: () => Promise<void> | void
  onRename: (next: string) => Promise<void> | void
  onDelete: () => Promise<void> | void
  ariaCheckboxLabel: string
  ariaDeleteLabel: string
  readOnly?: boolean
}

export function StepRow({ step, onToggle, onRename, onDelete, ariaCheckboxLabel, ariaDeleteLabel, readOnly = false }: StepRowProps) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(step.title)
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (!editing) setDraft(step.title)
  }, [step.title, editing])

  useEffect(() => {
    if (editing) {
      requestAnimationFrame(() => {
        inputRef.current?.focus()
        inputRef.current?.select()
      })
    }
  }, [editing])

  const commit = async () => {
    const next = draft.trim()
    if (next === '' || next === step.title) {
      setEditing(false)
      setDraft(step.title)
      return
    }
    setEditing(false)
    await onRename(next)
  }

  const cancel = () => {
    setEditing(false)
    setDraft(step.title)
  }

  return (
    <div className={styles.row} data-testid={`action-step-row-${step.id}`}>
      <button
        type="button"
        role="checkbox"
        aria-checked={step.done}
        aria-label={ariaCheckboxLabel}
        className={`${styles.checkbox} ${step.done ? styles.checkboxDone : ''}`}
        disabled={readOnly}
        onClick={(e) => {
          e.stopPropagation()
          void onToggle()
        }}
        data-testid={`action-step-checkbox-${step.id}`}
      >
        {step.done && (
          <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
            <polyline points="20 6 9 17 4 12" />
          </svg>
        )}
      </button>
      {readOnly ? (
        <span
          className={`${styles.title} ${styles.titleReadOnly} ${step.done ? styles.titleDone : ''}`}
          data-testid={`action-step-title-${step.id}`}
        >
          {step.title}
        </span>
      ) : editing ? (
        <input
          ref={inputRef}
          className={styles.titleEdit}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={commitOnEnter({
            onCommit: () => {
              void commit()
            },
            onCancel: cancel,
          })}
          onBlur={() => {
            void commit()
          }}
          data-testid={`action-step-edit-${step.id}`}
        />
      ) : (
        <button
          type="button"
          className={`${styles.title} ${step.done ? styles.titleDone : ''}`}
          onClick={() => setEditing(true)}
          data-testid={`action-step-title-${step.id}`}
        >
          {step.title}
        </button>
      )}
      {!readOnly && (
      <button
        type="button"
        className={styles.deleteButton}
        onClick={(e) => {
          e.stopPropagation()
          void onDelete()
        }}
        aria-label={ariaDeleteLabel}
        data-testid={`action-step-delete-${step.id}`}
      >
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <line x1="18" y1="6" x2="6" y2="18" />
          <line x1="6" y1="6" x2="18" y2="18" />
        </svg>
      </button>
      )}
    </div>
  )
}

interface AddStepRowProps {
  placeholder: string
  onSubmit: (title: string) => Promise<void> | void
}

export function AddStepRow({ placeholder, onSubmit }: AddStepRowProps) {
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (editing) {
      requestAnimationFrame(() => inputRef.current?.focus())
    }
  }, [editing])

  const commit = async () => {
    const next = draft.trim()
    if (next === '') {
      setEditing(false)
      setDraft('')
      return
    }
    setDraft('')
    setEditing(false)
    await onSubmit(next)
  }

  const cancel = () => {
    setEditing(false)
    setDraft('')
  }

  if (!editing) {
    return (
      <button
        type="button"
        className={styles.addRow}
        onClick={() => setEditing(true)}
        data-testid="action-step-add-button"
      >
        <span className={styles.addPlus} aria-hidden>
          <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
            <line x1="12" y1="5" x2="12" y2="19" />
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
        </span>
        <span className={styles.addPlaceholder}>{placeholder}</span>
      </button>
    )
  }

  return (
    <div className={styles.addRow}>
      <span className={styles.addPlus} aria-hidden>
        <svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
          <line x1="12" y1="5" x2="12" y2="19" />
          <line x1="5" y1="12" x2="19" y2="12" />
        </svg>
      </span>
      <input
        ref={inputRef}
        className={styles.addInput}
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        placeholder={placeholder}
        aria-label={placeholder}
        onKeyDown={commitOnEnter({
          onCommit: () => {
            void commit()
          },
          onCancel: cancel,
        })}
        onBlur={() => {
          void commit()
        }}
        data-testid="action-step-add-input"
      />
    </div>
  )
}
