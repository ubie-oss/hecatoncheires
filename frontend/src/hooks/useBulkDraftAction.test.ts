import { describe, expect, it } from 'vitest'
import type { GraphQLFormattedError } from 'graphql'
import { ApolloError } from '@apollo/client'

import {
  apolloErrorToBulkErrors,
  graphQLErrorToBulkError,
} from './useBulkDraftAction'
import { DRAFT_ERROR_CODE } from '../graphql/draftErrorCodes'

const mkGqlError = (
  code: string | undefined,
  message: string,
  extras: Record<string, unknown> = {},
): GraphQLFormattedError => ({
  message,
  extensions: code === undefined ? extras : { code, ...extras },
})

describe('graphQLErrorToBulkError', () => {
  it('parses MISSING_REQUIRED_FIELDS with missingFieldNames', () => {
    const e = mkGqlError('MISSING_REQUIRED_FIELDS', 'missing', {
      missingFieldNames: ['Severity', 'Reporter'],
    })
    const out = graphQLErrorToBulkError(e)
    expect(out.code).toBe(DRAFT_ERROR_CODE.MISSING_REQUIRED_FIELDS)
    if (out.code === DRAFT_ERROR_CODE.MISSING_REQUIRED_FIELDS) {
      expect(out.missingFieldNames).toEqual(['Severity', 'Reporter'])
    }
  })

  it('returns empty array when missingFieldNames is absent', () => {
    const e = mkGqlError('MISSING_REQUIRED_FIELDS', 'missing')
    const out = graphQLErrorToBulkError(e)
    if (out.code === DRAFT_ERROR_CODE.MISSING_REQUIRED_FIELDS) {
      expect(out.missingFieldNames).toEqual([])
    } else {
      throw new Error('expected MISSING_REQUIRED_FIELDS code')
    }
  })

  it('parses INVALID_STATUS_TRANSITION with currentStatus', () => {
    const e = mkGqlError('INVALID_STATUS_TRANSITION', 'not draft', {
      currentStatus: 'OPEN',
    })
    const out = graphQLErrorToBulkError(e)
    if (out.code === DRAFT_ERROR_CODE.INVALID_STATUS_TRANSITION) {
      expect(out.currentStatus).toBe('OPEN')
    } else {
      throw new Error('expected INVALID_STATUS_TRANSITION code')
    }
  })

  it('omits currentStatus when extension absent', () => {
    const e = mkGqlError('INVALID_STATUS_TRANSITION', 'not draft')
    const out = graphQLErrorToBulkError(e)
    if (out.code === DRAFT_ERROR_CODE.INVALID_STATUS_TRANSITION) {
      expect(out.currentStatus).toBeUndefined()
    } else {
      throw new Error('expected INVALID_STATUS_TRANSITION code')
    }
  })

  it('parses TITLE_REQUIRED', () => {
    const e = mkGqlError('TITLE_REQUIRED', 'title is required')
    expect(graphQLErrorToBulkError(e).code).toBe(DRAFT_ERROR_CODE.TITLE_REQUIRED)
  })

  it('parses FORBIDDEN', () => {
    const e = mkGqlError('FORBIDDEN', 'access denied')
    expect(graphQLErrorToBulkError(e).code).toBe(DRAFT_ERROR_CODE.FORBIDDEN)
  })

  it('parses NOT_FOUND', () => {
    const e = mkGqlError('NOT_FOUND', 'draft missing')
    expect(graphQLErrorToBulkError(e).code).toBe(DRAFT_ERROR_CODE.NOT_FOUND)
  })

  it('parses ACTIVATION_FAILED', () => {
    const e = mkGqlError('ACTIVATION_FAILED', 'slack failed')
    expect(graphQLErrorToBulkError(e).code).toBe(DRAFT_ERROR_CODE.ACTIVATION_FAILED)
  })

  it('parses FIELD_VALIDATION_FAILED', () => {
    const e = mkGqlError('FIELD_VALIDATION_FAILED', 'bad option id')
    expect(graphQLErrorToBulkError(e).code).toBe(DRAFT_ERROR_CODE.FIELD_VALIDATION_FAILED)
  })

  it('falls back to INTERNAL for unknown code', () => {
    const e = mkGqlError('SOMETHING_UNRECOGNIZED', 'huh')
    expect(graphQLErrorToBulkError(e).code).toBe(DRAFT_ERROR_CODE.INTERNAL)
  })

  it('falls back to INTERNAL when extensions is missing', () => {
    const e: GraphQLFormattedError = { message: 'no extensions' }
    expect(graphQLErrorToBulkError(e).code).toBe(DRAFT_ERROR_CODE.INTERNAL)
  })

  it('preserves the original message text', () => {
    const e = mkGqlError('TITLE_REQUIRED', 'precise wording from backend')
    expect(graphQLErrorToBulkError(e).message).toBe('precise wording from backend')
  })
})

describe('apolloErrorToBulkErrors', () => {
  it('returns empty for null/undefined', () => {
    expect(apolloErrorToBulkErrors(null)).toEqual([])
    expect(apolloErrorToBulkErrors(undefined)).toEqual([])
  })

  it('flattens multiple graphQLErrors in one ApolloError', () => {
    const err = new ApolloError({
      graphQLErrors: [
        mkGqlError('MISSING_REQUIRED_FIELDS', 'missing', {
          missingFieldNames: ['Severity'],
        }) as never,
        mkGqlError('ACTIVATION_FAILED', 'slack failed') as never,
      ],
    })
    const out = apolloErrorToBulkErrors(err)
    expect(out).toHaveLength(2)
    expect(out[0].code).toBe(DRAFT_ERROR_CODE.MISSING_REQUIRED_FIELDS)
    expect(out[1].code).toBe(DRAFT_ERROR_CODE.ACTIVATION_FAILED)
  })

  it('appends NETWORK error when networkError is present', () => {
    const err = new ApolloError({
      networkError: new Error('connection refused'),
    })
    const out = apolloErrorToBulkErrors(err)
    expect(out).toHaveLength(1)
    expect(out[0].code).toBe(DRAFT_ERROR_CODE.NETWORK)
    expect(out[0].message).toBe('connection refused')
  })

  it('surfaces both graphQLErrors and networkError', () => {
    const err = new ApolloError({
      graphQLErrors: [mkGqlError('NOT_FOUND', 'gone') as never],
      networkError: new Error('partial response'),
    })
    const out = apolloErrorToBulkErrors(err)
    expect(out).toHaveLength(2)
    expect(out.map((e) => e.code)).toEqual([
      DRAFT_ERROR_CODE.NOT_FOUND,
      DRAFT_ERROR_CODE.NETWORK,
    ])
  })

  // The server answers a rejected submitDraft with a non-2xx status and
  // data: null; Apollo then reports it as a ServerError with the errors on
  // networkError.result instead of graphQLErrors.
  it('classifies the GraphQL errors of a non-2xx response instead of reporting NETWORK', () => {
    const networkError = Object.assign(new Error('Response not successful: Received status code 400'), {
      name: 'ServerError',
      statusCode: 400,
      result: {
        data: null,
        errors: [mkGqlError('MISSING_REQUIRED_FIELDS', 'missing', { missingFieldNames: ['Severity'] })],
      },
    })
    const out = apolloErrorToBulkErrors(new ApolloError({ networkError }))
    expect(out).toEqual([
      { code: DRAFT_ERROR_CODE.MISSING_REQUIRED_FIELDS, message: 'missing', missingFieldNames: ['Severity'] },
    ])
  })

  it('falls back to INTERNAL for plain Error', () => {
    const out = apolloErrorToBulkErrors(new Error('boom'))
    expect(out).toHaveLength(1)
    expect(out[0].code).toBe(DRAFT_ERROR_CODE.INTERNAL)
    expect(out[0].message).toBe('boom')
  })
})
