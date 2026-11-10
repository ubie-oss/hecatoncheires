import { describe, expect, it } from 'vitest'
import { buildSlackCaseLink, buildSlackPermalink } from './slackLink'

const TEAM_CHANNEL = 'https://acme.slack.com/archives/C123'
const TS = '1700000000.123456'
const TS_COMPACT = 'p1700000000123456'

describe('buildSlackPermalink', () => {
  it('returns null without a timestamp', () => {
    expect(buildSlackPermalink(TEAM_CHANNEL, 'C123', null)).toBeNull()
    expect(buildSlackPermalink(TEAM_CHANNEL, 'C123', '')).toBeNull()
  })

  it('appends the compacted timestamp to the channel URL', () => {
    expect(buildSlackPermalink(TEAM_CHANNEL, 'C123', TS)).toBe(`${TEAM_CHANNEL}/${TS_COMPACT}`)
  })

  it('strips trailing slashes from the channel URL', () => {
    expect(buildSlackPermalink(`${TEAM_CHANNEL}//`, 'C123', TS)).toBe(`${TEAM_CHANNEL}/${TS_COMPACT}`)
  })

  it('falls back to the canonical slack.com host when the team URL is unknown', () => {
    expect(buildSlackPermalink(null, 'C123', TS)).toBe(`https://slack.com/archives/C123/${TS_COMPACT}`)
  })

  it('returns null when neither a channel URL nor a channel ID is known', () => {
    expect(buildSlackPermalink(null, null, TS)).toBeNull()
    expect(buildSlackPermalink(null, null, TS, '1700000000.000001')).toBeNull()
  })

  it('opens a thread reply within its thread', () => {
    expect(buildSlackPermalink(TEAM_CHANNEL, 'C123', TS, '1700000000.000001')).toBe(
      `${TEAM_CHANNEL}/${TS_COMPACT}?thread_ts=1700000000.000001&cid=C123`,
    )
    expect(buildSlackPermalink(null, 'C123', TS, '1700000000.000001')).toBe(
      `https://slack.com/archives/C123/${TS_COMPACT}?thread_ts=1700000000.000001&cid=C123`,
    )
  })

  it('treats a message that is its own thread root as a channel message', () => {
    for (const threadTS of [null, undefined, '', TS]) {
      expect(buildSlackPermalink(TEAM_CHANNEL, 'C123', TS, threadTS)).toBe(`${TEAM_CHANNEL}/${TS_COMPACT}`)
    }
  })
})

describe('buildSlackCaseLink', () => {
  it('returns null without a channel ID', () => {
    expect(buildSlackCaseLink(TEAM_CHANNEL, null, TS)).toBeNull()
    expect(buildSlackCaseLink(TEAM_CHANNEL, '', TS)).toBeNull()
  })

  it('links a thread-mode case to its thread, not to the monitored channel', () => {
    expect(buildSlackCaseLink(TEAM_CHANNEL, 'C123', TS)).toBe(`${TEAM_CHANNEL}/${TS_COMPACT}`)
  })

  it('links a thread-mode case via slack.com when the team URL is unknown', () => {
    expect(buildSlackCaseLink(null, 'C123', TS)).toBe(`https://slack.com/archives/C123/${TS_COMPACT}`)
  })

  it('links a channel-mode case to its own channel', () => {
    expect(buildSlackCaseLink('https://acme.slack.com/archives/C456', 'C456', null)).toBe(
      'https://acme.slack.com/archives/C456',
    )
  })

  it('strips trailing slashes from a channel-mode link', () => {
    expect(buildSlackCaseLink('https://acme.slack.com/archives/C456/', 'C456', null)).toBe(
      'https://acme.slack.com/archives/C456',
    )
  })

  it('links a channel-mode case via slack.com when the team URL is unknown', () => {
    expect(buildSlackCaseLink(null, 'C456', null)).toBe('https://slack.com/archives/C456')
  })

  it('always yields a link when a channel ID is known', () => {
    for (const threadTS of [null, undefined, '', TS]) {
      for (const channelURL of [null, undefined, TEAM_CHANNEL]) {
        expect(buildSlackCaseLink(channelURL, 'C123', threadTS)).not.toBeNull()
      }
    }
  })
})
