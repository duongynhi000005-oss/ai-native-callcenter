import { screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import type { Extension } from '@/lib/catalog'
import {
  notAllowed,
  pendingWrite,
  recordTexts,
  removeClipboard,
  selectedText,
  stubClipboard,
  stubExecCommand,
} from '@/test/clipboard'
import { renderWithProviders } from '@/test/harness'

import { Route } from './_app.admin.extensions'

/**
 * Copying a phone's SIP password (#290). The page is served over plain HTTP by
 * default, where the browser has no clipboard API, so every case here sets the
 * clipboard explicitly instead of inheriting the one `userEvent` installs.
 */

const Extensions = Route.options.component as () => ReactElement

// A hand-set password with the characters that break naive handling.
const PASSWORD = 'p@ss"w0rd\\`<x>&%20'

const EXTENSION: Extension = {
  id: '00000000-0000-4000-8000-0000000000e1',
  number: '1003',
  displayName: 'Desk 3',
  isEnabled: true,
  createdAt: '2026-10-20T08:00:00Z',
}

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function stubBackend() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input).replace('/api/v1', '')
      if (path === '/extensions') return json({ items: [EXTENSION] })
      if (path === `/extensions/${EXTENSION.id}/password`) return json({ password: PASSWORD })
      return json({ items: [] })
    }),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

async function revealPassword() {
  stubBackend()
  const view = renderWithProviders(<Extensions />)
  await view.user.click(await screen.findByTitle('Show password'))
  const dialog = await screen.findByRole('dialog')
  const shown = await within(dialog).findByText(PASSWORD)
  return { ...view, dialog, shown, copy: within(dialog).getByRole('button', { name: 'Copy' }) }
}

describe('copying a revealed password', () => {
  it('copies over plain HTTP, the exact password, and only then says so', async () => {
    const { user, copy } = await revealPassword()
    removeClipboard()
    const commands = stubExecCommand(true)

    await user.click(copy)

    expect(commands).toEqual([expect.objectContaining({ command: 'copy', selected: PASSWORD })])
    await waitFor(() => expect(copy).toHaveTextContent('Copied'))
  })

  it.each([
    ['refused', false],
    ['throws', 'throw'],
  ] as const)(
    'says it did not copy when the copy command is %s, and selects the password',
    async (_, result) => {
      const { user, copy, shown } = await revealPassword()
      removeClipboard()
      stubExecCommand(result)
      const texts = recordTexts(copy)

      await user.click(copy)

      await waitFor(() => expect(copy).toHaveTextContent('Not copied: press Ctrl+C (⌘C)'))
      expect(selectedText()).toBe(PASSWORD)
      expect(shown).toHaveTextContent(PASSWORD)
      texts.stop()
      expect(texts.seen.some((text) => text.includes('Copied'))).toBe(false)
    },
  )

  it('never says Copied, even briefly, when the browser refuses the write', async () => {
    const { user, copy } = await revealPassword()
    const pending = pendingWrite()
    stubClipboard(pending.write)
    const texts = recordTexts(copy)

    await user.click(copy)
    expect(copy).toHaveTextContent('Copy')
    pending.refuse(notAllowed())

    await waitFor(() => expect(copy).toHaveTextContent('Not copied'))
    expect(selectedText()).toBe(PASSWORD)
    texts.stop()
    expect(texts.seen.some((text) => text.includes('Copied'))).toBe(false)
  })

  it('says Copied only after the write settles in a secure context', async () => {
    const { user, copy } = await revealPassword()
    const pending = pendingWrite()
    const writeText = stubClipboard(pending.write)

    await user.click(copy)
    expect(writeText).toHaveBeenCalledWith(PASSWORD)
    expect(copy).toHaveTextContent('Copy')
    expect(copy).not.toHaveTextContent('Copied')

    pending.settle()
    await waitFor(() => expect(copy).toHaveTextContent('Copied'))
  })
})

describe('copying a freshly minted password', () => {
  it('copies the shown password over plain HTTP', async () => {
    stubBackend()
    const { user } = renderWithProviders(<Extensions />)
    await user.click(await screen.findByTitle('Edit'))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Reset' }))
    const shown = dialog.querySelector('code')!
    const minted = shown.textContent ?? ''
    expect(minted.length).toBeGreaterThanOrEqual(16)

    removeClipboard()
    const commands = stubExecCommand(true)
    const copy = within(dialog).getByRole('button', { name: 'Copy' })
    await user.click(copy)

    expect(commands).toEqual([expect.objectContaining({ command: 'copy', selected: minted })])
    await waitFor(() => expect(copy).toHaveTextContent('Copied'))
  })

  it('selects the shown password when the copy is refused', async () => {
    stubBackend()
    const { user } = renderWithProviders(<Extensions />)
    await user.click(await screen.findByTitle('Edit'))
    const dialog = await screen.findByRole('dialog')
    await user.click(within(dialog).getByRole('button', { name: 'Reset' }))
    const minted = dialog.querySelector('code')!.textContent ?? ''

    removeClipboard()
    stubExecCommand(false)
    const copy = within(dialog).getByRole('button', { name: 'Copy' })
    await user.click(copy)

    await waitFor(() => expect(copy).toHaveTextContent('Not copied'))
    expect(selectedText()).toBe(minted)
  })
})
