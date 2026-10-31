import { useCallback, useState } from 'react'
import { useApolloClient, type ApolloError, type ApolloClient } from '@apollo/client'
import type { GraphQLFormattedError } from 'graphql'

import { SUBMIT_DRAFT, DISCARD_DRAFT } from '../graphql/drafts'
import { graphQLErrorsFromApolloError } from '../utils/graphqlErrors'
import {
  DRAFT_ERROR_CODE,
  DRAFT_EXT_KEY,
  type BulkActionError,
  type DraftErrorCode,
} from '../graphql/draftErrorCodes'

export type DraftLite = { id: number; title: string }

export type BulkActionKind = 'submit' | 'discard'

export type BulkActionResult = {
  id: number
  title: string
  ok: boolean
  errors: BulkActionError[]
}

export type BulkRunState = {
  loading: boolean
  done: number
  total: number
}

export type BulkRunOptions = {
  workspaceId: string
  drafts: DraftLite[]
}

const KNOWN_CODES: ReadonlySet<DraftErrorCode> = new Set<DraftErrorCode>(
  Object.values(DRAFT_ERROR_CODE),
)

function isKnownCode(value: unknown): value is DraftErrorCode {
  return typeof value === 'string' && (KNOWN_CODES as Set<string>).has(value)
}

function stringArray(value: unknown): string[] {
  if (!Array.isArray(value)) return []
  return value.filter((v): v is string => typeof v === 'string')
}

function stringValue(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 ? value : undefined
}

// graphQLErrorToBulkError converts a single GraphQL error envelope into a
// fully-typed BulkActionError, branching on extensions.code to populate the
// code-specific detail fields. Unknown codes fall through to INTERNAL.
export function graphQLErrorToBulkError(err: GraphQLFormattedError): BulkActionError {
  const ext = (err.extensions ?? {}) as Record<string, unknown>
  const codeRaw = ext[DRAFT_EXT_KEY.CODE]
  const message = err.message || 'unknown error'

  if (isKnownCode(codeRaw)) {
    switch (codeRaw) {
      case DRAFT_ERROR_CODE.MISSING_REQUIRED_FIELDS:
        return {
          code: DRAFT_ERROR_CODE.MISSING_REQUIRED_FIELDS,
          message,
          missingFieldNames: stringArray(ext[DRAFT_EXT_KEY.MISSING_FIELD_NAMES]),
        }
      case DRAFT_ERROR_CODE.INVALID_STATUS_TRANSITION:
        return {
          code: DRAFT_ERROR_CODE.INVALID_STATUS_TRANSITION,
          message,
          currentStatus: stringValue(ext[DRAFT_EXT_KEY.CURRENT_STATUS]),
        }
      case DRAFT_ERROR_CODE.TITLE_REQUIRED:
      case DRAFT_ERROR_CODE.FIELD_VALIDATION_FAILED:
      case DRAFT_ERROR_CODE.FORBIDDEN:
      case DRAFT_ERROR_CODE.NOT_FOUND:
      case DRAFT_ERROR_CODE.ACTIVATION_FAILED:
      case DRAFT_ERROR_CODE.NETWORK:
      case DRAFT_ERROR_CODE.INTERNAL:
        return { code: codeRaw, message }
    }
  }
  return { code: DRAFT_ERROR_CODE.INTERNAL, message }
}

// apolloErrorToBulkErrors flattens an ApolloError into the per-error
// records the result dialog renders. Both graphQLErrors and networkError
// (when present) are surfaced so a single mutation that fails on multiple
// axes does not get its error list silently truncated. A networkError that
// is only the non-2xx status of a response carrying GraphQL errors is not
// reported again as NETWORK: those errors already describe the failure.
export function apolloErrorToBulkErrors(err: unknown): BulkActionError[] {
  if (!err) return []
  const out: BulkActionError[] = []

  const apollo = err as Partial<ApolloError> & { message?: string }
  const gqlErrors = graphQLErrorsFromApolloError(err)
  for (const ge of gqlErrors) {
    out.push(graphQLErrorToBulkError(ge))
  }
  const network = apollo.networkError
  const errorsCameFromNetworkResult = (apollo.graphQLErrors?.length ?? 0) === 0 && gqlErrors.length > 0
  if (network && !errorsCameFromNetworkResult) {
    const msg = (network as Error).message || 'network error'
    out.push({ code: DRAFT_ERROR_CODE.NETWORK, message: msg })
  }
  if (out.length === 0) {
    const msg = (err as Error)?.message ?? 'unknown error'
    out.push({ code: DRAFT_ERROR_CODE.INTERNAL, message: msg })
  }
  return out
}

async function runSingle(
  client: ApolloClient<unknown>,
  kind: BulkActionKind,
  workspaceId: string,
  draft: DraftLite,
): Promise<BulkActionResult> {
  try {
    if (kind === 'submit') {
      await client.mutate({
        mutation: SUBMIT_DRAFT,
        variables: { workspaceId, id: draft.id },
      })
    } else {
      await client.mutate({
        mutation: DISCARD_DRAFT,
        variables: { workspaceId, id: draft.id },
      })
    }
    return { id: draft.id, title: draft.title, ok: true, errors: [] }
  } catch (err) {
    return {
      id: draft.id,
      title: draft.title,
      ok: false,
      errors: apolloErrorToBulkErrors(err),
    }
  }
}

// useBulkDraftAction performs a per-draft loop of submitDraft / discardDraft
// mutations and aggregates the outcomes. It uses ApolloClient.mutate
// directly (instead of useMutation) because useMutation's single-flight
// behaviour does not compose with manually orchestrating N calls.
//
// **Execution is sequential, not parallel.** SubmitDraft's activation
// tail runs a series of Slack API calls (CreateChannel → InviteUsers →
// AddBookmark → PostMessage → GetConversationMembers) per case. When two
// of those tails are in flight at the same time we have seen the invite
// step silently drop for one of the cases — partly because Slack tiers
// the invite endpoint and partly because invite errors are funnelled
// through errutil.Handle on the server, hiding the failure from the
// user. Serialising the bulk loop keeps each case's tail isolated; it
// trades wall-clock latency for predictability, which is the right
// tradeoff at the bulk-action scale we expect.
//
// Discard does not touch Slack, but we serialise it too for uniform
// progress reporting and because the per-call cost is tiny.
//
// The hook does NOT issue a refetch — that decision belongs to the caller
// so it can stage UI updates (close confirm dialog, then refetch, then
// open result dialog) in whatever order suits the page.
export function useBulkDraftAction(): {
  state: BulkRunState
  run: (kind: BulkActionKind, opts: BulkRunOptions) => Promise<BulkActionResult[]>
} {
  const client = useApolloClient()
  const [state, setState] = useState<BulkRunState>({ loading: false, done: 0, total: 0 })

  const run = useCallback(
    async (kind: BulkActionKind, opts: BulkRunOptions): Promise<BulkActionResult[]> => {
      const total = opts.drafts.length
      if (total === 0) {
        return []
      }
      setState({ loading: true, done: 0, total })

      const results: BulkActionResult[] = []
      for (const d of opts.drafts) {
        const r = await runSingle(client, kind, opts.workspaceId, d)
        results.push(r)
        setState({ loading: true, done: results.length, total })
      }
      setState({ loading: false, done: results.length, total })
      return results
    },
    [client],
  )

  return { state, run }
}
