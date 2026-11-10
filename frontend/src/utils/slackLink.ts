// Slack archive URL / permalink composition.
//
// Slack accepts archive URLs of the form {channelURL}/p{ts-without-dot}, and
// also tolerates the canonical https://slack.com/archives/... fallback when
// the workspace subdomain isn't known. The workspace-qualified form is
// preferred wherever the team URL is available because it names the target
// workspace unambiguously.

// buildSlackPermalink composes a permalink to a specific message in a channel.
// When the message is a reply inside a thread, pass the thread's root
// timestamp as threadTS: Slack then opens the reply within its thread instead
// of looking for it at the channel root.
export function buildSlackPermalink(
  channelURL: string | null | undefined,
  channelID: string | null | undefined,
  ts: string | null | undefined,
  threadTS?: string | null,
): string | null {
  if (!ts) return null
  const tsCompact = ts.replace('.', '')
  let base: string
  if (channelURL) {
    base = `${trimTrailingSlash(channelURL)}/p${tsCompact}`
  } else if (channelID) {
    base = `https://slack.com/archives/${channelID}/p${tsCompact}`
  } else {
    return null
  }
  if (!threadTS || threadTS === ts) return base
  const params = new URLSearchParams({ thread_ts: threadTS })
  if (channelID) params.set('cid', channelID)
  return `${base}?${params.toString()}`
}

// buildSlackCaseLink composes the link that represents a Case in Slack.
//
// A thread-mode Case lives in a thread of a monitored channel, so its channel
// alone is the wrong destination — the link must carry the thread timestamp.
// A channel-mode Case owns its channel, so the channel archive URL is the
// destination.
//
// Invariant: a non-empty channelID always yields a non-null result. Callers
// deciding whether a cell renders its own <a> rely on this.
export function buildSlackCaseLink(
  channelURL: string | null | undefined,
  channelID: string | null | undefined,
  threadTS: string | null | undefined,
): string | null {
  if (!channelID) return null
  if (threadTS) return buildSlackPermalink(channelURL, channelID, threadTS)
  if (channelURL) return trimTrailingSlash(channelURL)
  return `https://slack.com/archives/${channelID}`
}

function trimTrailingSlash(url: string): string {
  return url.replace(/\/+$/, '')
}
