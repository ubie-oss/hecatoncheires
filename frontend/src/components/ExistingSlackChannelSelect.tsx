import { useEffect, useRef, useState } from 'react'
import { useApolloClient, useQuery } from '@apollo/client'
import Select, { components } from 'react-select'
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

const menuNoteStyle = { fontSize: 11.5, color: 'var(--fg-muted)', padding: 'var(--spacing-xs) var(--spacing-md-sm)' }

// Appends the "more channels match" note under the options, where the user is
// looking when the list is cut off. react-select forwards unknown props to
// custom components through selectProps, which is how the note reaches it.
const MenuList = (props: any) => (
  <components.MenuList {...props}>
    {props.children}
    {props.selectProps.moreMatchesNote && (
      <div style={menuNoteStyle} data-testid="existing-slack-channel-has-more">{props.selectProps.moreMatchesNote}</div>
    )}
  </components.MenuList>
)

interface CandidateListData {
  existingSlackChannelCandidates: {
    channels: SlackChannelCandidate[]
    hasMore: boolean
    fetchedAt: string
  }
}

// How long typing must pause before the typed text is sent as a search. The
// server searches the user's whole channel list, so a request per keystroke
// would repeat that work for text the user is still changing.
const SEARCH_DEBOUNCE_MS = 250

const hintStyle = { fontSize: 11.5, color: 'var(--fg-muted)', marginTop: 4 }
const errorStyle = { color: 'var(--danger)', fontSize: 12, marginTop: 4 }
// A link-like text button: reloading is an occasional repair, so it should not
// compete with the picker for attention.
const textButtonStyle = {
  background: 'none',
  border: 'none',
  padding: 0,
  font: 'inherit',
  color: 'inherit',
  textDecoration: 'underline',
  cursor: 'pointer',
}

export default function ExistingSlackChannelSelect({
  workspaceId,
  value,
  onChange,
  caseLabelLower,
  error,
  refetchKey,
}: ExistingSlackChannelSelectProps) {
  const { t, lang } = useTranslation()
  const client = useApolloClient()
  const [inputValue, setInputValue] = useState('')
  const [query, setQuery] = useState('')
  const [reloading, setReloading] = useState(false)
  const [reloadFailed, setReloadFailed] = useState(false)

  useEffect(() => {
    const timer = setTimeout(() => setQuery(inputValue.trim()), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(timer)
  }, [inputValue])

  // Links change between two openings of the form, so every search goes to
  // the server; the server keeps the Slack channel list itself.
  const { data, previousData, loading, error: queryError, refetch } = useQuery<CandidateListData>(
    GET_EXISTING_SLACK_CHANNEL_CANDIDATES,
    {
      variables: { workspaceId, query },
      fetchPolicy: 'network-only',
      notifyOnNetworkStatusChange: true,
    },
  )

  const initialRefetchKey = useRef(refetchKey)
  useEffect(() => {
    if (refetchKey !== initialRefetchKey.current) {
      void refetch()
    }
  }, [refetchKey, refetch])

  // Fetches the channel list from Slack, then repeats the current search on
  // it. The fetch is a separate request rather than refetch({ refresh: true })
  // because Apollo keeps refetch variables for the searches that follow, and
  // each of those would then list every channel from Slack again.
  const reloadFromSlack = async () => {
    setReloading(true)
    setReloadFailed(false)
    try {
      await client.query({
        query: GET_EXISTING_SLACK_CHANNEL_CANDIDATES,
        variables: { workspaceId, query, refresh: true },
        fetchPolicy: 'network-only',
      })
      await refetch()
    } catch {
      setReloadFailed(true)
    } finally {
      setReloading(false)
    }
  }

  const errorLine = error && <div style={errorStyle} data-testid="existing-slack-channel-error">{error}</div>
  const list = (data ?? previousData)?.existingSlackChannelCandidates

  if (!list && (loading || reloading)) {
    return (
      <div data-testid="existing-slack-channel-select">
        <div style={hintStyle}>{t('loadingChannels')}</div>
        {errorLine}
      </div>
    )
  }

  if (!list) {
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

  const options: ChannelOption[] = list.channels.map((c) => ({ value: c.id, label: c.name, candidate: c }))
  const selected = value ? { value: value.id, label: value.name, candidate: value } : null
  const busy = loading || reloading
  const noChannelsAtAll = !busy && !queryError && !reloadFailed && query === '' && list.channels.length === 0
  const fetchedAt = new Date(list.fetchedAt).toLocaleString(lang === 'ja' ? 'ja-JP' : 'en-US', {
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })

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
        inputValue={inputValue}
        onInputChange={(text) => setInputValue(text)}
        // The server has already searched; filtering again here would hide
        // matches while the next search is on its way.
        filterOption={null}
        isLoading={busy}
        loadingMessage={() => t('loadingChannels')}
        placeholder={t('placeholderSelectChannel')}
        noOptionsMessage={() => (inputValue ? t('noMatchingChannels') : t('emptyExistingChannels'))}
        components={{ Option, SingleValue, MenuList }}
        {...({ moreMatchesNote: list.hasMore ? t('hintMoreChannelsMatch') : '' } as object)}
      />
      {(queryError || reloadFailed) && (
        <div style={errorStyle} data-testid="existing-slack-channel-load-error">{t('errorLoadChannels')}</div>
      )}
      {noChannelsAtAll ? (
        <>
          <div style={hintStyle} data-testid="existing-slack-channel-empty">{t('emptyExistingChannels')}</div>
          <div style={{ marginTop: 6 }}>
            <Button variant="ghost" onClick={() => { void reloadFromSlack() }} disabled={busy} data-testid="existing-slack-channel-retry">
              {t('existingChannelsRetry')}
            </Button>
          </div>
        </>
      ) : (
        <div style={hintStyle}>
          <span data-testid="existing-slack-channel-fetched-at">{t('hintChannelListFetchedAt', { time: fetchedAt })}</span>
          {' · '}
          <button
            type="button"
            style={textButtonStyle}
            onClick={() => { void reloadFromSlack() }}
            disabled={busy}
            data-testid="existing-slack-channel-reload"
          >
            {t('reloadChannelList')}
          </button>
        </div>
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
