import type { GraphQLFormattedError } from 'graphql'

type ApolloErrorLike = {
  graphQLErrors?: readonly GraphQLFormattedError[]
  networkError?: { result?: unknown } | null
}

// graphQLErrorsFromApolloError returns the GraphQL errors a failed operation's
// response carried. The server answers a rejected operation with a non-2xx
// status, and Apollo Client 3 then reports it as a ServerError whose
// graphQLErrors stays empty unless the body also carried data, so the errors
// are read from the parsed body on networkError.result as well.
export function graphQLErrorsFromApolloError(err: unknown): readonly GraphQLFormattedError[] {
  const e = err as ApolloErrorLike | null | undefined
  if (e?.graphQLErrors && e.graphQLErrors.length > 0) {
    return e.graphQLErrors
  }
  const result = e?.networkError?.result
  if (result && typeof result === 'object') {
    const errors = (result as { errors?: unknown }).errors
    if (Array.isArray(errors)) {
      return errors as GraphQLFormattedError[]
    }
  }
  return []
}
