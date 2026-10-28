import { useEffect, useRef } from 'react'
import { useQuery } from '@apollo/client'
import Select, { components, createFilter } from 'react-select'
import { GET_EXISTING_SLACK_CHANNEL_CANDIDATES } from '../graphql/case'
import { useTranslation } from '../i18n'
import Button from './Button'
import { IconLock } from './Icons'
import { buildSelectStyles, portalProps } from './selectStyles'

export interface SlackChannelCandidate {
  id: string
  name: string
  isPrivate: boolean
}

interface ChannelOption {
  value: string
  label: string
  candidate: SlackChannelCandidate
}

interface ExistingSlackChannelSelectProps {
  workspaceId: string
  value: SlackChannelCandidate | null
  onChange: (candidate: SlackChannelCandidate | null) => void
  // Lower-cased case label, interpolated into the visibility hint.
  caseLabelLower: string
  error?: string
  // Incrementing it re-fetches the candidates, e.g. after the server rejected
  // the chosen channel because another case took it in the meantime.
  refetchKey: number
}

const ChannelName = ({ candidate }: { candidate: SlackChannelCandidate }) => (
  <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
    {candidate.isPrivate ? (
      <IconLock size={12} sw={2} aria-hidden data-testid="existing-slack-channel-private-icon" />
    ) : (
      <span aria-hidden>#</span>
    )}
    <span>{candidate.name}</span>
  </span>
)

const Option = (props: any) => (
  <components.Option {...props}>
    <ChannelName candidate={props.data.candidate} />
  </components.Option>
)

const SingleValue = (props: any) => (
  <components.SingleValue {...props}>
    <ChannelName candidate={props.data.candidate} />
  </components.SingleValue>
)

// Matches the typed text against the channel name only (not the ID),
// ignoring case.
const filterByName = createFilter<ChannelOption>({
  ignoreCase: true,
  stringify: (option) => option.label,
})

const hintStyle = { fontSize: 11.5, color: 'var(--fg-muted)', marginTop: 4 }
const errorStyle = { color: 'var(--danger)', fontSize: 12, marginTop: 4 }

export default function ExistingSlackChannelSelect({
  workspaceId,
  value,
  onChange,
  caseLabelLower,
  error,
  refetchKey,
}: ExistingSlackChannelSelectProps) {
  const { t } = useTranslation()
  const { data, loading, error: queryError, refetch } = useQuery(GET_EXISTING_SLACK_CHANNEL_CANDIDATES, {
    variables: { workspaceId },
    fetchPolicy: 'network-only',
    notifyOnNetworkStatusChange: true,
  })

  const initialRefetchKey = useRef(refetchKey)
  useEffect(() => {
    if (refetchKey !== initialRefetchKey.current) {
      void refetch()
    }
  }, [refetchKey, refetch])

  const errorLine = error && <div style={errorStyle} data-testid="existing-slack-channel-error">{error}</div>

  if (loading) {
    return (
      <div data-testid="existing-slack-channel-select">
        <div style={hintStyle}>{t('loadingChannels')}</div>
        {errorLine}
      </div>
    )
  }

  if (queryError) {
    return (
      <div data-testid="existing-slack-channel-select">
        <div style={errorStyle} data-testid="existing-slack-channel-load-error">{t('errorLoadChannels')}</div>
        <div style={{ marginTop: 6 }}>
          <Button variant="ghost" onClick={() => { void refetch() }} data-testid="existing-slack-channel-retry">
            {t('existingChannelsRetry')}
          </Button>
        </div>
        {errorLine}
      </div>
    )
  }

  const candidates: SlackChannelCandidate[] = data?.existingSlackChannelCandidates ?? []
  const options: ChannelOption[] = candidates.map((c) => ({ value: c.id, label: c.name, candidate: c }))
  const selected = value ? { value: value.id, label: value.name, candidate: value } : null

  return (
    <div data-testid="existing-slack-channel-select">
      <Select<ChannelOption, false>
        {...portalProps}
        inputId="existing-slack-channel"
        aria-label={t('labelSlackChannel')}
        styles={buildSelectStyles({ error: !!error }) as any}
        options={options}
        value={selected}
        onChange={(opt) => onChange(opt ? opt.candidate : null)}
        filterOption={filterByName}
        placeholder={t('placeholderSelectChannel')}
        noOptionsMessage={({ inputValue }) => (inputValue ? t('noMatchingChannels') : t('emptyExistingChannels'))}
        components={{ Option, SingleValue }}
      />
      {candidates.length === 0 ? (
        <>
          <div style={hintStyle} data-testid="existing-slack-channel-empty">{t('emptyExistingChannels')}</div>
          <div style={{ marginTop: 6 }}>
            <Button variant="ghost" onClick={() => { void refetch() }} data-testid="existing-slack-channel-retry">
              {t('existingChannelsRetry')}
            </Button>
          </div>
        </>
      ) : (
        <div style={hintStyle}>{t('hintExistingChannelScope')}</div>
      )}
      {value && (
        <div style={hintStyle} data-testid="existing-slack-channel-visibility">
          {value.isPrivate
            ? t('hintChannelVisibilityPrivate', { caseLabelLower })
            : t('hintChannelVisibilityPublic', { caseLabelLower })}
        </div>
      )}
      {errorLine}
    </div>
  )
}
