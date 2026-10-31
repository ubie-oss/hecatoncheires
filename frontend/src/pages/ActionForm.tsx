import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client'
import Select from 'react-select'
import UserSelect from '../components/UserSelect'
import { buildSelectStyles, portalProps } from '../components/selectStyles'
import { CREATE_ACTION, UPDATE_ACTION, GET_ACTIONS, GET_ACTION, GET_OPEN_CASE_ACTIONS } from '../graphql/action'
import { GET_CASE, GET_CASES } from '../graphql/case'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'
import { useWorkspace } from '../contexts/workspace-context'
import { useTranslation } from '../i18n'
import { useActionStatuses } from '../hooks/useActionStatuses'
import { useAssigneeCandidates } from '../hooks/useAssigneeCandidates'
import Modal from '../components/Modal'
import Button from '../components/Button'
import { displayName } from '../utils/user'
import { graphQLErrorsFromApolloError } from '../utils/graphqlErrors'

interface ActionItem {
  id: number
  caseID: number
  title: string
  description: string
  status: string
  assigneeID: string | null
  dueDate?: string | null
}

interface ActionFormProps {
  action: ActionItem | null
  defaultCaseID?: number
  onClose: () => void
}

export default function ActionForm({ action, defaultCaseID, onClose }: ActionFormProps) {
  const { currentWorkspace } = useWorkspace()
  const { t } = useTranslation()
  const { statuses, initialId, label } = useActionStatuses(currentWorkspace?.id)

  const isEdit = action !== null

  const [title, setTitle] = useState(action?.title || '')
  const [description, setDescription] = useState(action?.description || '')
  const [caseID, setCaseID] = useState<number | null>(action?.caseID ?? defaultCaseID ?? null)
  const [status, setStatus] = useState(action?.status || initialId)
  const [assigneeID, setAssigneeID] = useState<string | null>(action?.assigneeID ?? null)
  const [errors, setErrors] = useState<Record<string, string>>({})

  const { users } = useAssigneeCandidates(currentWorkspace?.id)
  const userOptions = users.map((u: any) => ({
    value: u.id as string,
    label: displayName(u),
    name: u.name,
    realName: u.realName,
    imageUrl: u.imageUrl,
  }))
  const selectedAssignee = userOptions.find((o: any) => o.value === assigneeID) || null

  const { data: casesData } = useQuery(GET_CASES, {
    variables: { workspaceId: currentWorkspace?.id, status: 'OPEN' },
    skip: !currentWorkspace,
  })
  const { data: configData } = useQuery(GET_FIELD_CONFIGURATION, {
    variables: { workspaceId: currentWorkspace?.id },
    skip: !currentWorkspace,
  })
  const caseLabel = configData?.fieldConfiguration?.labels?.case || 'Case'

  const createRefetch: any[] = [
    { query: GET_ACTIONS, variables: { workspaceId: currentWorkspace?.id } },
    { query: GET_OPEN_CASE_ACTIONS, variables: { workspaceId: currentWorkspace?.id } },
  ]
  if (defaultCaseID) {
    // CaseDetail defaults to the 'open' view (actionsFilter='ACTIVE');
    // matching that exactly is what makes Apollo's refetchQueries actually
    // refresh the page after mutating an action.
    createRefetch.push({
      query: GET_CASE,
      variables: { workspaceId: currentWorkspace?.id, id: defaultCaseID, actionsFilter: 'ACTIVE' },
    })
  }
  const [createAction, { loading: creating }] = useMutation(CREATE_ACTION, {
    refetchQueries: createRefetch,
  })
  const updateRefetch: any[] = [
    { query: GET_OPEN_CASE_ACTIONS, variables: { workspaceId: currentWorkspace?.id } },
  ]
  if (action) {
    updateRefetch.push({
      query: GET_ACTION,
      variables: { workspaceId: currentWorkspace?.id, id: action.id },
    })
    updateRefetch.push({
      query: GET_CASE,
      variables: { workspaceId: currentWorkspace?.id, id: action.caseID, actionsFilter: 'ACTIVE' },
    })
  }
  const [updateAction, { loading: updating }] = useMutation(UPDATE_ACTION, {
    refetchQueries: updateRefetch,
  })

  const submit = async () => {
    const errs: Record<string, string> = {}
    if (!title.trim()) errs.title = t('errorTitleRequired')
    if (!isEdit && !caseID) errs.caseID = t('errorCaseRequired')
    if (Object.keys(errs).length > 0) { setErrors(errs); return }

    try {
      if (isEdit && action) {
        const updateInput: Record<string, unknown> = { id: action.id, title, description, status }
        if (assigneeID) {
          updateInput.assigneeID = assigneeID
        } else {
          updateInput.clearAssignee = true
        }
        await updateAction({
          variables: {
            workspaceId: currentWorkspace!.id,
            input: updateInput,
          },
        })
      } else {
        const createInput: Record<string, unknown> = { caseID: Number(caseID), title, description, status }
        if (assigneeID) {
          createInput.assigneeID = assigneeID
        }
        await createAction({
          variables: {
            workspaceId: currentWorkspace!.id,
            input: createInput,
          },
        })
      }
      onClose()
    } catch (e: any) {
      console.error('Action mutation failed', e)
      const msg = graphQLErrorsFromApolloError(e)[0]?.message || e?.message || String(e)
      setErrors({ submit: msg })
    }
  }

  const submitting = creating || updating
  const cases = casesData?.cases || []
  const caseOptions = cases.map((c: any) => ({
    value: c.id as number,
    label: c.accessDenied ? `#${c.id}` : `#${c.id} ${c.title}`,
  }))
  const selectedCase = caseOptions.find((o: any) => o.value === caseID) || null

  return (
    <Modal
      open
      onClose={onClose}
      width={560}
      title={isEdit ? t('titleActionFormEdit') : t('titleActionFormNew')}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>{t('btnCancel')}</Button>
          <Button variant="primary" onClick={submit} disabled={submitting}>
            {submitting ? (isEdit ? t('btnSaving') : t('btnCreating')) : (isEdit ? t('btnSave') : t('btnCreate'))}
          </Button>
        </>
      }
    >
      <div className="col" style={{ gap: 14 }}>
        {!isEdit && (
          <div>
            <label htmlFor="action-case" className="field-label">{t('labelCaseRequired', { caseLabel })}</label>
            <Select
              inputId="action-case"
              aria-label={caseLabel}
              options={caseOptions}
              value={selectedCase}
              onChange={(opt: any) => {
                setCaseID(opt ? opt.value : null)
                if (errors.caseID) setErrors((p) => { const n = { ...p }; delete n.caseID; return n })
              }}
              placeholder={t('placeholderSelectCase', { caseLabelLower: caseLabel.toLowerCase() })}
              isClearable
              classNamePrefix="rs"
              {...portalProps}
              styles={buildSelectStyles()}
            />
            {errors.caseID && <div style={{ color: 'var(--danger)', fontSize: 12, marginTop: 4 }}>{errors.caseID}</div>}
          </div>
        )}
        <div>
          <label htmlFor="title" className="field-label">{t('labelTitleRequired')}</label>
          <input
            id="title"
            className="input"
            value={title}
            onChange={(e) => { setTitle(e.target.value); if (errors.title) setErrors((p) => ({ ...p, title: '' })) }}
            placeholder={t('placeholderActionTitle')}
          />
          {errors.title && <div style={{ color: 'var(--danger)', fontSize: 12, marginTop: 4 }}>{errors.title}</div>}
        </div>
        <div>
          <label htmlFor="description" className="field-label">{t('labelDescription')}</label>
          <textarea
            id="description"
            className="textarea"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder={t('placeholderActionDescription')}
          />
        </div>
        <div>
          <label htmlFor="action-assignee" className="field-label">{t('labelAssignee')}</label>
          <UserSelect
            inputId="action-assignee"
            aria-label={t('labelAssignee')}
            isClearable
            options={userOptions}
            value={selectedAssignee}
            onChange={(opt: any) => setAssigneeID(opt ? opt.value : null)}
            placeholder={t('placeholderSelectAssignee')}
          />
        </div>
        {isEdit && (
          <div>
            <label htmlFor="action-status" className="field-label">{t('labelStatusRequired')}</label>
            <select
              id="action-status"
              className="select"
              value={status}
              onChange={(e) => setStatus(e.target.value)}
            >
              {statuses.map((s) => (
                <option key={s.id} value={s.id}>{label(s.id)}</option>
              ))}
            </select>
          </div>
        )}
        {errors.submit && (
          <div style={{
            padding: '8px 10px',
            borderRadius: 6,
            background: 'color-mix(in oklch, var(--danger) 10%, transparent)',
            border: '1px solid color-mix(in oklch, var(--danger) 30%, transparent)',
            color: 'var(--danger)',
            fontSize: 12,
          }}>
            {errors.submit}
          </div>
        )}
      </div>
    </Modal>
  )
}
