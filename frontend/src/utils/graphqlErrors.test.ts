import { describe, expect, it } from 'vitest'
import { ApolloError } from '@apollo/client'
import { GraphQLError } from 'graphql'
import { graphQLErrorsFromApolloError } from './graphqlErrors'

describe('graphQLErrorsFromApolloError', () => {
  it('returns graphQLErrors when Apollo populated them', () => {
    const err = new ApolloError({
      graphQLErrors: [new GraphQLError('rejected', { extensions: { code: 'NOT_FOUND' } })],
    })
    const errors = graphQLErrorsFromApolloError(err)
    expect(errors.map((e) => e.extensions?.code)).toEqual(['NOT_FOUND'])
  })

  it('reads the errors of a non-2xx response from networkError.result', () => {
    const networkError = Object.assign(new Error('Response not successful: Received status code 500'), {
      name: 'ServerError',
      statusCode: 500,
      result: {
        data: null,
        errors: [{ message: 'join failed', extensions: { code: 'SLACK_CHANNEL_JOIN_FAILED' } }],
      },
    })
    const errors = graphQLErrorsFromApolloError(new ApolloError({ networkError }))
    expect(errors).toEqual([{ message: 'join failed', extensions: { code: 'SLACK_CHANNEL_JOIN_FAILED' } }])
  })

  it('returns an empty list when the response carried no GraphQL errors', () => {
    expect(graphQLErrorsFromApolloError(new ApolloError({ networkError: new Error('offline') }))).toEqual([])
    expect(graphQLErrorsFromApolloError(new Error('plain'))).toEqual([])
    expect(graphQLErrorsFromApolloError(undefined)).toEqual([])
  })
})
