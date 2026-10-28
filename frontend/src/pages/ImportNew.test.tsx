import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import '@testing-library/jest-dom/vitest'
import { MockedProvider, type MockedResponse } from '@apollo/client/testing'
import { MemoryRouter } from 'react-router'
import { I18nProvider } from '../i18n'
import { GET_FIELD_CONFIGURATION } from '../graphql/fieldConfiguration'
import { CREATE_CASE_IMPORT } from '../graphql/import'
import ImportNew from './ImportNew'

const WORKSPACE_ID = 'risk'
const SESSION_ID = '11111111-2222-3333-4444-555555555555'
const YAML = 'version: 1\ncases:\n  - title: pasted case\n'
// `|+` keeps every trailing newline as part of the description value, so
// this fixture only round-trips if the content is submitted verbatim.
const YAML_KEEP_CHOMPING =
  'version: 1\ncases:\n  - title: pasted case\n    description: |+\n      first line\n\n\n'

vi.mock('../contexts/workspace-context', () => ({
  useWorkspace: () => ({
    currentWorkspace: { id: WORKSPACE_ID, name: 'Risk' },
    workspaces: [{ id: WORKSPACE_ID, name: 'Risk' }],
    isLoading: false,
    setCurrentWorkspace: vi.fn(),
    switchWorkspace: vi.fn(),
  }),
}))

const navigateSpy = vi.fn()
vi.mock('react-router', async (importOriginal) => {
  const actual = await importOriginal<typeof import('react-router')>()
  return { ...actual, useNavigate: () => navigateSpy }
})

function fieldConfigMock(): MockedResponse {
  return {
    request: { query: GET_FIELD_CONFIGURATION, variables: { workspaceId: WORKSPACE_ID } },
    result: {
      data: {
        fieldConfiguration: {
          fields: [],
          labels: { case: 'Case' },
          actionConfig: { initial: 'BACKLOG', closed: ['COMPLETED'], statuses: [] },
          allowExistingSlackChannel: false,
        },
      },
    },
  }
}

// The mock only matches when `input` carries `content` alone — pasted YAML has
// no file, so `originalFileName` must be omitted rather than sent as null. The
// content is matched exactly, which pins the submitted YAML to the paste box's
// verbatim text.
function createImportMock(
  input: { content: string; originalFileName?: string },
  onCall?: () => void,
): MockedResponse {
  return {
    request: {
      query: CREATE_CASE_IMPORT,
      variables: { workspaceId: WORKSPACE_ID, input },
    },
    result: () => {
      onCall?.()
      return {
        data: {
          createCaseImport: {
            id: SESSION_ID,
            workspaceID: WORKSPACE_ID,
            creatorUserID: 'U1',
            status: 'PENDING',
            source: {
              originalFileName: input.originalFileName ?? '',
              sizeBytes: input.content.length,
            },
            issues: [],
            valid: true,
            fieldSchemaHash: 'hash',
            createdAt: '2026-07-28T00:00:00Z',
            updatedAt: '2026-07-28T00:00:00Z',
            executedAt: null,
            createdCount: 0,
            failedCount: 0,
            skippedCount: 0,
            snapshot: { version: 1, cases: [] },
          },
        },
      }
    },
  }
}

function renderPage(mocks: MockedResponse[]) {
  return render(
    <MockedProvider mocks={mocks} addTypename={false}>
      <MemoryRouter>
        <I18nProvider>
          <ImportNew />
        </I18nProvider>
      </MemoryRouter>
    </MockedProvider>,
  )
}

/** Dispatch a page-level paste (target = document, not an input). */
function dispatchDocumentPaste(text: string) {
  const event = new Event('paste', { bubbles: true, cancelable: true })
  Object.defineProperty(event, 'clipboardData', {
    value: { getData: () => text, files: [] },
  })
  fireEvent(document, event)
}

describe('ImportNew', () => {
  beforeEach(() => {
    navigateSpy.mockClear()
  })
  afterEach(() => {
    cleanup()
  })

  it('starts in file mode with the dropzone visible and no paste box', () => {
    renderPage([fieldConfigMock()])
    expect(screen.getByText('Drop a YAML file here')).toBeInTheDocument()
    expect(screen.queryByTestId('import-paste-textarea')).not.toBeInTheDocument()
  })

  it('submits pasted YAML without an originalFileName and navigates to the session', async () => {
    renderPage([fieldConfigMock(), createImportMock({ content: YAML })])

    fireEvent.click(screen.getByTestId('import-mode-paste'))
    fireEvent.change(screen.getByTestId('import-paste-textarea'), {
      target: { value: YAML },
    })
    fireEvent.click(screen.getByTestId('import-paste-submit'))

    await waitFor(() => {
      expect(navigateSpy).toHaveBeenCalledWith(
        `/ws/${WORKSPACE_ID}/imports/${SESSION_ID}`,
      )
    })
  })

  it('submits the pasted YAML verbatim, keeping trailing newlines a block scalar owns', async () => {
    renderPage([fieldConfigMock(), createImportMock({ content: YAML_KEEP_CHOMPING })])

    fireEvent.click(screen.getByTestId('import-mode-paste'))
    fireEvent.change(screen.getByTestId('import-paste-textarea'), {
      target: { value: YAML_KEEP_CHOMPING },
    })
    fireEvent.click(screen.getByTestId('import-paste-submit'))

    // The mock matches the exact content, so a successful navigate proves
    // the trailing blank lines survived instead of being trimmed away.
    await waitFor(() => {
      expect(navigateSpy).toHaveBeenCalledWith(
        `/ws/${WORKSPACE_ID}/imports/${SESSION_ID}`,
      )
    })
  })

  it('keeps the submit button disabled until the paste box has content', () => {
    renderPage([fieldConfigMock()])

    fireEvent.click(screen.getByTestId('import-mode-paste'))
    expect(screen.getByTestId('import-paste-submit')).toBeDisabled()

    fireEvent.change(screen.getByTestId('import-paste-textarea'), {
      target: { value: '   \n  ' },
    })
    expect(screen.getByTestId('import-paste-submit')).toBeDisabled()

    fireEvent.change(screen.getByTestId('import-paste-textarea'), {
      target: { value: YAML },
    })
    expect(screen.getByTestId('import-paste-submit')).toBeEnabled()
  })

  it('captures a page-level paste into the paste box without submitting', async () => {
    renderPage([fieldConfigMock()])

    dispatchDocumentPaste(YAML)

    const textarea = await screen.findByTestId('import-paste-textarea')
    expect(textarea).toHaveValue(YAML)
    // Capturing must not fire the mutation — the user still confirms.
    expect(navigateSpy).not.toHaveBeenCalled()
  })

  it('ignores a page-level paste with no text content', () => {
    renderPage([fieldConfigMock()])

    dispatchDocumentPaste('   \n')

    expect(screen.queryByTestId('import-paste-textarea')).not.toBeInTheDocument()
  })

  it('ignores further input while the chosen file is still being read', async () => {
    let resolveRead: ((content: string) => void) | undefined
    const file = {
      name: 'incidents.yaml',
      text: () =>
        new Promise<string>((resolve) => {
          resolveRead = resolve
        }),
    } as unknown as File

    let mutationCalls = 0
    renderPage([
      fieldConfigMock(),
      createImportMock(
        { content: YAML, originalFileName: 'incidents.yaml' },
        () => {
          mutationCalls += 1
        },
      ),
    ])

    const fileInput = document.querySelector('input[type="file"]')
    expect(fileInput).not.toBeNull()
    fireEvent.change(fileInput as HTMLInputElement, { target: { files: [file] } })

    // While the read is outstanding the page must refuse a second entry —
    // otherwise two createCaseImport calls race and the page lands on
    // whichever session happened to resolve first.
    dispatchDocumentPaste('version: 1\ncases: []\n')
    expect(screen.queryByTestId('import-paste-textarea')).not.toBeInTheDocument()

    await act(async () => {
      resolveRead?.(YAML)
    })

    await waitFor(() => {
      expect(navigateSpy).toHaveBeenCalledWith(
        `/ws/${WORKSPACE_ID}/imports/${SESSION_ID}`,
      )
    })
    expect(mutationCalls).toBe(1)
    expect(navigateSpy).toHaveBeenCalledTimes(1)
  })
})
