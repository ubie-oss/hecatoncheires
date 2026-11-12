import { describe, expect, it } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import type { ReactNode } from 'react'
import { GET_ACTIONS_ENABLED } from '../graphql/action'
import { useActionsEnabled } from './useActionsEnabled'

function enabledMock(workspaceId: string, enabled: boolean): MockedResponse {
  return {
    request: { query: GET_ACTIONS_ENABLED, variables: { workspaceId } },
    result: { data: { actionsEnabled: enabled } },
  }
}

function wrapperFor(mocks: MockedResponse[]) {
  return ({ children }: { children: ReactNode }) => (
    <MockedProvider mocks={mocks} addTypename={false}>
      {children}
    </MockedProvider>
  )
}

describe('useActionsEnabled', () => {
  it('returns true when the workspace has Actions', async () => {
    const { result } = renderHook(() => useActionsEnabled('ws-a'), {
      wrapper: wrapperFor([enabledMock('ws-a', true)]),
    })
    expect(result.current).toBeUndefined()
    await waitFor(() => expect(result.current).toBe(true))
  })

  it('returns false when the workspace leaves Actions off', async () => {
    const { result } = renderHook(() => useActionsEnabled('ws-b'), {
      wrapper: wrapperFor([enabledMock('ws-b', false)]),
    })
    await waitFor(() => expect(result.current).toBe(false))
  })

  it('stays undefined when the query fails', async () => {
    const failing: MockedResponse = {
      request: { query: GET_ACTIONS_ENABLED, variables: { workspaceId: 'ws-c' } },
      error: new Error('boom'),
    }
    const { result } = renderHook(() => useActionsEnabled('ws-c'), {
      wrapper: wrapperFor([failing]),
    })
    // Give the failed request a chance to settle; the value must not turn into false.
    await new Promise((r) => setTimeout(r, 20))
    expect(result.current).toBeUndefined()
  })

  it('skips the query without a workspace', () => {
    const { result } = renderHook(() => useActionsEnabled(undefined), {
      wrapper: wrapperFor([]),
    })
    expect(result.current).toBeUndefined()
  })
})
