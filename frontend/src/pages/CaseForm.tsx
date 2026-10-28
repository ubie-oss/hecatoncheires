import { useState } from 'react'
import { useNavigate } from 'react-router'
import { useMutation, useQuery } from '@apollo/client'
import UserSelect from '../components/UserSelect'
import ExistingSlackChannelSelect, { type SlackChannelCandidate } from '../components/ExistingSlackChannelSelect'
import { CREATE_CASE, UPDATE_CASE, ASSIGN_CASE, UNASSIGN_CASE, GET_CASE, GET_CASES } from '../graphql/case'
import { CREATE_DRAFT, SUBMIT_DRAFT, GET_DRAFTS } from '../graphql/drafts'
import { diffAssignees } from '../utils/assignees'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'
import { useWorkspace } from '../contexts/workspace-context'
import { useCaseStatuses } from '../hooks/useCaseStatuses'
import { useAssigneeCandidates } from '../hooks/useAssigneeCandidates'
import { useTranslation } from '../i18n'
import Modal from '../components/Modal'
import Button from '../components/Button'
import CustomFieldRenderer from '../components/fields/CustomFieldRenderer'
import { IconLock, IconFlask } from '../components/Icons'
import { sanitizeFieldValues } from '../utils/sanitizeFieldValues'
import { displayName } from '../utils/user'

interface CaseItem {
  id: number
  title: string
  description: string
  isPrivate: boolean
  isTest?: boolean
  assigneeIDs: string[]
  fields: Array<{ fieldId: string; value: any }>
  // Drives the footer button set. `'DRAFT'` switches to the Save / Submit
  // pair (overwrite draft vs. promote to OPEN). Missing/undefined keeps
  // the regular Edit-an-existing-OPEN-case behaviour.
  status?: 'DRAFT' | 'OPEN' | 'CLOSED'
}

// GraphQL extensions.code values the create mutation returns when the chosen
// existing channel is rejected; each is shown under the channel picker.
const existingChannelErrorKeys = {
  SLACK_CHANNEL_ALREADY_LINKED: 'errorChannelAlreadyLinked',
  SLACK_CHANNEL_NOT_LINKABLE: 'errorChannelNotLinkable',
  SLACK_CHANNEL_JOIN_FAILED: 'errorChannelJoinFailed',
} as const

interface CaseFormProps {
  caseItem: CaseItem | null
  onClose: () => void
  // Called after a successful Submit-from-draft promotion. CaseDetail
  // uses this to drop its draft-edit modal once the case is OPEN.
  onSubmitted?: () => void
}

export default function CaseForm({ caseItem, onClose, onSubmitted }: CaseFormProps) {
  const { currentWorkspace } = useWorkspace()
  const { t } = useTranslation()
  const navigate = useNavigate()

  // Private cases are a channel-mode-only feature: a private case creates a
  // dedicated private Slack channel, whereas thread-mode cases reuse the
  // monitored channel and never carry IsPrivate. Only show the private toggle
  // once we have POSITIVELY confirmed channel mode. isThreadMode is false for
  // channel-mode, the in-flight query, a failed query, AND a skipped query
  // alike, so guarding on isThreadMode alone would fail open in several ways:
  //   - flashing the toggle on while the config is still loading;
  //   - leaving it on when the config query errored (mode unknown);
  //   - flashing it on during the initial workspace-context load, when
  //     currentWorkspace is undefined so useCaseStatuses SKIPS its query and a
  //     skipped Apollo query reports loading:false / error:undefined.
  // Requiring a resolved workspace id plus !loading && !error closes all three.
  const caseStatuses = useCaseStatuses(currentWorkspace?.id)
  const showPrivateToggle =
    !!currentWorkspace?.id &&
    !caseStatuses.loading &&
    !caseStatuses.error &&
    !caseStatuses.isThreadMode

  const isEdit = caseItem !== null
  const isDraftEdit = isEdit && caseItem?.status === 'DRAFT'
  const [title, setTitle] = useState(caseItem?.title || '')
  const [description, setDescription] = useState(caseItem?.description || '')
  const [isPrivate, setIsPrivate] = useState(caseItem?.isPrivate ?? false)
  const [isTest, setIsTest] = useState(caseItem?.isTest ?? false)
  const [assigneeIDs, setAssigneeIDs] = useState<string[]>(caseItem?.assigneeIDs || [])
  const [fieldValues, setFieldValues] = useState<Record<string, any>>(() => {
    const map: Record<string, any> = {}
    caseItem?.fields?.forEach((f) => { map[f.fieldId] = f.value })
    return map
  })
  const [errors, setErrors] = useState<Record<string, string>>({})
  // Where the new case's Slack channel comes from. 'existing' links a channel
  // the user picks; its privacy then follows the channel, so the private
  // toggle is hidden and isPrivate is not sent. isPrivate keeps its value so
  // switching back to 'new' restores what the user had chosen.
  const [channelSource, setChannelSource] = useState<'new' | 'existing'>('new')
  const [existingChannel, setExistingChannel] = useState<SlackChannelCandidate | null>(null)
  const [candidatesRefetchKey, setCandidatesRefetchKey] = useState(0)

  const { data: configData } = useQuery(GET_FIELD_CONFIGURATION, {
    variables: { workspaceId: currentWorkspace?.id },
    skip: !currentWorkspace,
  })
  const showChannelSource =
    !isEdit && showPrivateToggle && !!configData?.fieldConfiguration?.allowExistingSlackChannel
  const usingExistingChannel = showChannelSource && channelSource === 'existing'
  const { users } = useAssigneeCandidates(currentWorkspace?.id)

  const [createCase, { loading: creating }] = useMutation(CREATE_CASE, {
    refetchQueries: [{ query: GET_CASES, variables: { workspaceId: currentWorkspace?.id, status: 'OPEN' } }],
  })
  const [updateCase, { loading: updating }] = useMutation(UPDATE_CASE, {
    refetchQueries: caseItem
      ? [
          { query: GET_CASE, variables: { workspaceId: currentWorkspace?.id, id: caseItem.id, actionsFilter: 'ACTIVE' } },
          // The Drafts tab on Case List needs to reflect title / field
          // edits made through the modal.
          ...(isDraftEdit ? [{ query: GET_DRAFTS, variables: { workspaceId: currentWorkspace?.id } }] : []),
        ]
      : [],
  })
  const [createDraft, { loading: savingDraft }] = useMutation(CREATE_DRAFT, {
    refetchQueries: [{ query: GET_DRAFTS, variables: { workspaceId: currentWorkspace?.id } }],
  })
  const [submitDraft, { loading: submittingDraft }] = useMutation(SUBMIT_DRAFT, {
    refetchQueries: caseItem
      ? [
          { query: GET_DRAFTS, variables: { workspaceId: currentWorkspace?.id } },
          { query: GET_CASE, variables: { workspaceId: currentWorkspace?.id, id: caseItem.id, actionsFilter: 'ACTIVE' } },
          { query: GET_CASES, variables: { workspaceId: currentWorkspace?.id, status: 'OPEN' } },
        ]
      : [],
    awaitRefetchQueries: true,
  })
  const [assignCase] = useMutation(ASSIGN_CASE)
  const [unassignCase] = useMutation(UNASSIGN_CASE)

  // reconcileAssignees persists the modal's assignee selection through the
  // delta assign/unassign mutations. Assignees are not part of the
  // updateCase / submitDraft inputs (a full-list replace there could clobber a
  // concurrent edit), so on the edit paths we diff the picker selection
  // against the case's original assignees and apply only the change.
  const reconcileAssignees = async (id: number) => {
    if (!caseItem) return
    const { toAdd, toRemove } = diffAssignees(caseItem.assigneeIDs ?? [], assigneeIDs)
    if (toAdd.length > 0) {
      await assignCase({ variables: { workspaceId: currentWorkspace!.id, id, userIDs: toAdd } })
    }
    if (toRemove.length > 0) {
      await unassignCase({ variables: { workspaceId: currentWorkspace!.id, id, userIDs: toRemove } })
    }
  }

  const fields = configData?.fieldConfiguration?.fields || []
  const caseLabel = configData?.fieldConfiguration?.labels?.case || 'Case'

  const handleFieldChange = (fieldID: string, value: any) => {
    setFieldValues((prev) => ({ ...prev, [fieldID]: value }))
    if (errors[fieldID]) {
      setErrors((prev) => { const n = { ...prev }; delete n[fieldID]; return n })
    }
  }

  const collectFieldArr = () =>
    sanitizeFieldValues(
      Object.entries(fieldValues)
        .filter(([, v]) => v !== undefined && v !== null && v !== '')
        .map(([fieldId, value]) => ({ fieldId, value })),
      fields,
    )

  // handleSubmit drives the primary action: Create (new), Save (edit
  // existing OPEN/CLOSED case), or Submit (promote a DRAFT to OPEN).
  // For Submit-from-DRAFT the modal first saves the latest edits via
  // UpdateCase, then calls SubmitDraft so the backend sees the freshly
  // edited data when re-running its required-field check.
  const handleSubmit = async () => {
    const errs: Record<string, string> = {}
    if (!title.trim()) errs.title = t('errorTitleRequired')
    fields.forEach((f: any) => {
      if (f.required && (fieldValues[f.id] === undefined || fieldValues[f.id] === null || fieldValues[f.id] === '')) {
        errs[f.id] = t('errorFieldRequired', { fieldName: f.name })
      }
    })
    if (usingExistingChannel && !existingChannel) errs.existingChannel = t('errorSelectChannel')
    if (Object.keys(errs).length > 0) { setErrors(errs); return }

    const fieldArr = collectFieldArr()

    try {
      if (isDraftEdit && caseItem) {
        // Single mutation: edits + promote run inside one usecase call so
        // required-field validation and channel-activation see the same
        // payload. Splitting these across two roundtrips would let a
        // failed promotion leave an already-edited DRAFT behind.
        await reconcileAssignees(caseItem.id)
        await submitDraft({
          variables: {
            workspaceId: currentWorkspace!.id,
            id: caseItem.id,
            input: {
              title,
              description,
              isTest,
              fields: fieldArr,
            },
          },
        })
        onSubmitted?.()
        onClose()
      } else if (isEdit && caseItem) {
        await updateCase({
          variables: {
            workspaceId: currentWorkspace!.id,
            input: {
              id: caseItem.id,
              title,
              description,
              isTest,
              fields: fieldArr,
            },
          },
        })
        await reconcileAssignees(caseItem.id)
        onClose()
      } else {
        const common = { title, description, isTest, assigneeIDs, fields: fieldArr }
        await createCase({
          variables: {
            workspaceId: currentWorkspace!.id,
            input: usingExistingChannel && existingChannel
              ? { ...common, existingSlackChannelID: existingChannel.id }
              : { ...common, isPrivate },
          },
        })
        onClose()
      }
    } catch (e: any) {
      console.error('Case mutation failed', e)
      const code = e?.graphQLErrors?.[0]?.extensions?.code as keyof typeof existingChannelErrorKeys | undefined
      if (usingExistingChannel && code && code in existingChannelErrorKeys) {
        setErrors({ existingChannel: t(existingChannelErrorKeys[code], { caseLabelLower: caseLabel.toLowerCase() }) })
        // The chosen channel may have been taken or become unusable; drop it
        // from the list by fetching the candidates again.
        setCandidatesRefetchKey((k) => k + 1)
        return
      }
      const msg = e?.graphQLErrors?.[0]?.message || e?.message || String(e)
      setErrors({ submit: msg })
    }
  }

  // handleSaveDraftOverwrite persists the in-modal edits back onto an
  // existing DRAFT without promoting it — required-field validation is
  // skipped because half-finished entries are exactly the point of the
  // DRAFT state.
  const handleSaveDraftOverwrite = async () => {
    if (!isDraftEdit || !caseItem || !currentWorkspace) return
    const fieldArr = collectFieldArr()
    try {
      await updateCase({
        variables: {
          workspaceId: currentWorkspace.id,
          input: {
            id: caseItem.id,
            title,
            description,
            isTest,
            fields: fieldArr,
          },
        },
      })
      await reconcileAssignees(caseItem.id)
      onClose()
    } catch (e: any) {
      console.error('Draft overwrite failed', e)
      const msg = e?.graphQLErrors?.[0]?.message || e?.message || String(e)
      setErrors({ submit: msg })
    }
  }

  // handleSaveAsDraft creates a brand-new DRAFT from the in-flight form
  // state. Used only on the "New case" path; the draft-edit path goes
  // through handleSaveDraftOverwrite instead so the existing case row
  // is mutated in place.
  const handleSaveAsDraft = async () => {
    if (!currentWorkspace) return
    const fieldArr = collectFieldArr()
    try {
      await createDraft({
        variables: {
          workspaceId: currentWorkspace.id,
          input: {
            title: title || null,
            description: description || null,
            isPrivate,
            isTest,
            assigneeIDs,
            fields: fieldArr,
          },
        },
      })
      onClose()
      // The Drafts tab on the Case list page surfaces newly saved drafts.
      navigate(`/ws/${currentWorkspace.id}/cases`)
    } catch (e: any) {
      console.error('Save-as-draft mutation failed', e)
      const msg = e?.graphQLErrors?.[0]?.message || e?.message || String(e)
      setErrors({ submit: msg })
    }
  }

  const userOptions = users.map((u) => ({
    value: u.id,
    label: displayName(u),
    name: u.name,
    realName: u.realName,
    imageUrl: u.imageUrl,
  }))
  const selectedAssignees = userOptions.filter((o) => assigneeIDs.includes(o.value))

  const submitting = creating || updating
  const busy = submitting || savingDraft || submittingDraft

  const primaryLabel = (() => {
    if (isDraftEdit) {
      return submittingDraft || updating ? t('btnSubmittingDraft') : t('draftSubmitButton')
    }
    if (isEdit) {
      return submitting ? t('btnSaving') : t('btnSave')
    }
    return submitting ? t('btnCreating') : t('btnCreate')
  })()

  return (
    <Modal
      open
      onClose={onClose}
      width={580}
      title={isEdit ? t('titleCaseFormEdit', { caseLabel }) : t('titleCaseFormNew', { caseLabel })}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>{t('btnCancel')}</Button>
          {!isEdit && (
            <Button
              variant="ghost"
              onClick={handleSaveAsDraft}
              disabled={busy || usingExistingChannel}
              data-testid="case-save-as-draft-button"
            >
              {savingDraft ? t('btnSavingDraft') : t('btnSaveAsDraft')}
            </Button>
          )}
          {isDraftEdit && (
            <Button
              variant="ghost"
              onClick={handleSaveDraftOverwrite}
              disabled={busy}
              data-testid="draft-save-button"
            >
              {updating && !submittingDraft ? t('btnSavingDraft') : t('btnSaveAsDraft')}
            </Button>
          )}
          <Button
            variant="primary"
            onClick={handleSubmit}
            disabled={busy}
            data-testid="case-submit-button"
          >
            {primaryLabel}
          </Button>
        </>
      }
    >
      <div className="col" style={{ gap: 14 }}>
        <div>
          <div className="field-label">{t('labelTitleRequired')}</div>
          <input
            className="input"
            value={title}
            onChange={(e) => { setTitle(e.target.value); if (errors.title) setErrors((p) => ({ ...p, title: '' })) }}
            placeholder={t('placeholderCaseTitle', { caseLabelLower: caseLabel.toLowerCase() })}
            data-testid="case-title-input"
          />
          {errors.title && <div style={{ color: 'var(--danger)', fontSize: 12, marginTop: 4 }}>{errors.title}</div>}
        </div>
        <div>
          <div className="field-label">{t('labelDescription')}</div>
          <textarea
            className="textarea"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            placeholder={t('placeholderCaseDescription', { caseLabelLower: caseLabel.toLowerCase() })}
            data-testid="case-description-input"
          />
        </div>
        <div>
          <label htmlFor="case-assignees" className="field-label">{t('labelAssignees')}</label>
          <UserSelect
            inputId="case-assignees"
            aria-label={t('labelAssignees')}
            isMulti
            options={userOptions}
            value={selectedAssignees}
            onChange={(opts: any) => setAssigneeIDs((opts || []).map((o: any) => o.value))}
            placeholder={t('placeholderSelectAssignees')}
          />
        </div>
        {fields.length > 0 && (
          <div>
            <div className="field-label">{t('sectionFields')}</div>
            <div className="col" style={{ gap: 12 }}>
              {fields.map((f: any) => (
                <CustomFieldRenderer
                  key={f.id}
                  field={f}
                  value={fieldValues[f.id]}
                  onChange={handleFieldChange}
                  users={users}
                  error={errors[f.id]}
                  disabled={busy}
                />
              ))}
            </div>
          </div>
        )}
        {/* Test-case toggle: editable on both create and edit so a case can be
            marked or un-marked as a test at any point. */}
        <label className="row" style={{ gap: 8, padding: 10, border: '1px solid color-mix(in oklch, var(--info) 30%, var(--line))', borderRadius: 6, background: 'color-mix(in oklch, var(--info) 8%, transparent)', alignItems: 'flex-start', cursor: 'pointer' }}>
          <input
            type="checkbox"
            checked={isTest}
            onChange={(e) => setIsTest(e.target.checked)}
            style={{ marginTop: 2 }}
            data-testid="test-case-checkbox"
          />
          <div>
            <div className="row" style={{ gap: 6, fontSize: 13, fontWeight: 500 }}>
              <IconFlask size={12} sw={2} />
              {t('labelTestCase', { caseLabel })}
            </div>
            <div style={{ fontSize: 11.5, color: 'var(--fg-muted)', marginTop: 2 }}>
              {t('hintTestCase', { caseLabelLower: caseLabel.toLowerCase() })}
            </div>
          </div>
        </label>
        {showChannelSource && (
          <div data-testid="case-channel-source">
            <div className="field-label">{t('labelSlackChannel')}</div>
            <div className="col" style={{ gap: 6 }}>
              <label className="row" style={{ gap: 8, fontSize: 13, cursor: 'pointer' }}>
                <input
                  type="radio"
                  name="case-channel-source"
                  checked={channelSource === 'new'}
                  onChange={() => {
                    setChannelSource('new')
                    setErrors((p) => ({ ...p, existingChannel: '' }))
                  }}
                  data-testid="case-channel-source-new"
                />
                {t('optionCreateNewChannel')}
              </label>
              <label className="row" style={{ gap: 8, fontSize: 13, cursor: 'pointer' }}>
                <input
                  type="radio"
                  name="case-channel-source"
                  checked={channelSource === 'existing'}
                  onChange={() => setChannelSource('existing')}
                  data-testid="case-channel-source-existing"
                />
                {t('optionUseExistingChannel')}
              </label>
            </div>
            {usingExistingChannel && currentWorkspace && (
              <div style={{ marginTop: 8 }}>
                <ExistingSlackChannelSelect
                  workspaceId={currentWorkspace.id}
                  value={existingChannel}
                  onChange={(c) => {
                    setExistingChannel(c)
                    setErrors((p) => ({ ...p, existingChannel: '' }))
                  }}
                  caseLabelLower={caseLabel.toLowerCase()}
                  error={errors.existingChannel || undefined}
                  refetchKey={candidatesRefetchKey}
                />
                <div style={{ fontSize: 11.5, color: 'var(--fg-muted)', marginTop: 4 }} data-testid="case-draft-unavailable-hint">
                  {t('hintDraftUnavailableWithExistingChannel')}
                </div>
              </div>
            )}
          </div>
        )}
        {!isEdit && showPrivateToggle && !usingExistingChannel && (
          <label className="row" style={{ gap: 8, padding: 10, border: '1px solid color-mix(in oklch, var(--warn) 30%, var(--line))', borderRadius: 6, background: 'color-mix(in oklch, var(--warn) 8%, transparent)', alignItems: 'flex-start', cursor: 'pointer' }}>
            <input
              type="checkbox"
              checked={isPrivate}
              onChange={(e) => setIsPrivate(e.target.checked)}
              style={{ marginTop: 2 }}
              data-testid="private-case-checkbox"
            />
            <div>
              <div className="row" style={{ gap: 6, fontSize: 13, fontWeight: 500 }}>
                <IconLock size={12} sw={2} />
                {t('labelPrivateCase', { caseLabel })}
              </div>
              <div style={{ fontSize: 11.5, color: 'var(--fg-muted)', marginTop: 2 }}>
                {t('hintPrivateCase', { caseLabelLower: caseLabel.toLowerCase() })}
              </div>
            </div>
          </label>
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
