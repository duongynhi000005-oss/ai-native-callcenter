import { screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import {
  notAllowed,
  recordTexts,
  removeClipboard,
  selectedText,
  stubClipboard,
  stubExecCommand,
} from '@/test/clipboard'
import { renderWithProviders } from '@/test/harness'

import { Route } from './_app.admin.keys'

/**
 * Copying an API key's secret from the once-only dialog (#290). The secret
 * cannot be shown again once the dialog closes, so a copy that silently did
 * nothing, or said "Copied" when it had not, loses the key.
 */

const Keys = Route.options.component as () => ReactElement

// As the server mints it: 32 random bytes, base64url.
const SECRET = 'k3Vq9ZtR-mW1xY_uH7bN2cL8pJ4sF6dG0aE5iO3rTqU'

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function stubBackend() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const path = String(input).replace('/api/v1', '')
      if (path === '/api-keys' && init.method === 'POST') {
        return json(
          {
            id: '00000000-0000-4000-8000-0000000000b1',
            name: 'crm-sync',
            keyPrefix: SECRET.slice(0, 8),
            status: 'ACTIVE',
            scopes: [],
            createdAt: '2026-10-21T02:00:00Z',
            secret: SECRET,
          },
          201,
        )
      }
      return json({ items: [] })
    }),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

async function issueKey() {
  stubBackend()
  const view = renderWithProviders(<Keys />)
  await view.user.click(await screen.findByRole('button', { name: 'Issue key' }))
  const form = await screen.findByRole('dialog')
  await view.user.type(within(form).getByLabelText(/^name$/i), 'crm-sync')
  await view.user.click(within(form).getByRole('button', { name: 'Issue key' }))
  const dialog = await screen.findByRole('dialog', { name: 'crm-sync is issued' })
  return { ...view, dialog, copy: within(dialog).getByRole('button', { name: 'Copy' }) }
}

describe('copying an issued secret', () => {
  it('copies over plain HTTP, the whole secret, and only then says so', async () => {
    const { user, copy } = await issueKey()
    removeClipboard()
    const commands = stubExecCommand(true)

    await user.click(copy)

    expect(commands).toEqual([expect.objectContaining({ command: 'copy', selected: SECRET })])
    await waitFor(() => expect(copy).toHaveTextContent('Copied'))
  })

  it('says it did not copy and selects the secret when the copy command fails', async () => {
    const { user, copy, dialog } = await issueKey()
    removeClipboard()
    stubExecCommand(false)

    await user.click(copy)

    await waitFor(() => expect(copy).toHaveTextContent('Not copied: press Ctrl+C (⌘C)'))
    expect(selectedText()).toBe(SECRET)
    // Still on screen: the dialog is the only place it will ever be.
    expect(within(dialog).getAllByText(SECRET, { exact: false }).length).toBeGreaterThan(0)
  })

  it('never says Copied when the browser refuses the write', async () => {
    const { user, copy } = await issueKey()
    stubClipboard(() => Promise.reject(notAllowed()))
    const texts = recordTexts(copy)

    await user.click(copy)

    await waitFor(() => expect(copy).toHaveTextContent('Not copied'))
    expect(selectedText()).toBe(SECRET)
    texts.stop()
    expect(texts.seen.some((text) => text.includes('Copied'))).toBe(false)
  })

  it('copies through the clipboard API where there is one', async () => {
    const { user, copy } = await issueKey()
    const writeText = stubClipboard(async () => {})

    await user.click(copy)

    expect(writeText).toHaveBeenCalledWith(SECRET)
    await waitFor(() => expect(copy).toHaveTextContent('Copied'))
  })
})
