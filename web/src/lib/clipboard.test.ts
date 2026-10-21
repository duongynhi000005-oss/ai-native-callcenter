import { afterEach, describe, expect, it } from 'vitest'

import {
  notAllowed,
  pendingWrite,
  removeClipboard,
  stubClipboard,
  stubExecCommand,
} from '@/test/clipboard'

import { copyText, selectContents } from './clipboard'

/**
 * #290: copying works over plain HTTP, where there is no clipboard API, and
 * the answer says whether the copy happened rather than whether it was tried.
 */

// Values as they really are, not `abc`: a hand-set SIP password with the
// characters that break naive handling, a 64-character one, an API key secret
// as the server mints it (32 random bytes, base64url) and a UUIDv7 call id.
const PASSWORD = 'p@ss"w0rd\\`<x>&%20'
const LONG_PASSWORD = 'Zr8#qL2!vN6^tB4&xK9*mH1(pD7)sF3_wJ5+yC0=gT8-aU2~eR6|iO4;nM1:bV7Q'
const API_SECRET = 'k3Vq9ZtR-mW1xY_uH7bN2cL8pJ4sF6dG0aE5iO3rTqU'
const CALL_ID = '019a0b7c-3e21-7d4f-8a9b-2c3d4e5f6a7b'

afterEach(() => {
  document.body.innerHTML = ''
})

describe('without a clipboard API (plain HTTP)', () => {
  it.each([PASSWORD, LONG_PASSWORD, API_SECRET, CALL_ID])(
    'copies %s exactly through the copy command',
    async (value) => {
      removeClipboard()
      const commands = stubExecCommand(true)

      expect(await copyText(value)).toBe(true)
      expect(commands).toHaveLength(1)
      expect(commands[0].command).toBe('copy')
      expect(commands[0].selected).toBe(value)
      // Nothing is left behind in the page.
      expect(document.querySelector('textarea')).toBeNull()
    },
  )

  it('says it did not copy when the command is refused', async () => {
    removeClipboard()
    const commands = stubExecCommand(false)
    expect(await copyText(PASSWORD)).toBe(false)
    expect(commands).toHaveLength(1)
    expect(document.querySelector('textarea')).toBeNull()
  })

  it('says it did not copy when the command throws, and does not throw itself', async () => {
    removeClipboard()
    stubExecCommand('throw')
    await expect(copyText(PASSWORD)).resolves.toBe(false)
    expect(document.querySelector('textarea')).toBeNull()
  })

  it('says it did not copy when there is no copy command either', async () => {
    removeClipboard()
    // jsdom has no execCommand, like a browser that dropped it.
    await expect(copyText(PASSWORD)).resolves.toBe(false)
  })

  it('selects beside the focused element and gives focus back', async () => {
    removeClipboard()
    // A dialog's focus trap pulls focus back from anything outside it, so the
    // textarea goes where focus already is.
    document.body.innerHTML = '<div role="dialog"><span><button>Copy</button></span></div>'
    const button = document.querySelector('button')!
    button.focus()
    const commands = stubExecCommand(true)

    expect(await copyText(API_SECRET)).toBe(true)
    expect(commands[0].parent).toBe(button.parentElement)
    expect(document.activeElement).toBe(button)
  })
})

describe('with a clipboard API (a secure context)', () => {
  it('copies through it, and only says so once the write has settled', async () => {
    const pending = pendingWrite()
    const writeText = stubClipboard(pending.write)
    const commands = stubExecCommand(true)

    let answer: boolean | undefined
    const copying = copyText(CALL_ID).then((isCopied) => (answer = isCopied))
    await Promise.resolve()
    expect(answer).toBeUndefined()

    pending.settle()
    await copying
    expect(answer).toBe(true)
    expect(writeText).toHaveBeenCalledWith(CALL_ID)
    expect(commands).toHaveLength(0)
  })

  it('says it did not copy when the write is refused', async () => {
    stubClipboard(() => Promise.reject(notAllowed()))
    const commands = stubExecCommand(true)
    expect(await copyText(PASSWORD)).toBe(false)
    // A refusal is an answer, not a reason to try another way behind the
    // reader's back.
    expect(commands).toHaveLength(0)
  })
})

describe('selectContents', () => {
  it("selects an element's whole text", () => {
    document.body.innerHTML = `<code>${API_SECRET}</code>`
    selectContents(document.querySelector('code'))
    expect(String(window.getSelection())).toBe(API_SECRET)
  })

  it('selects the whole value of a field', () => {
    document.body.innerHTML = '<input readonly>'
    const input = document.querySelector('input')!
    input.value = PASSWORD
    selectContents(input)
    expect(document.activeElement).toBe(input)
    expect(input.value.slice(input.selectionStart!, input.selectionEnd!)).toBe(PASSWORD)
  })

  it('does nothing without an element', () => {
    expect(() => selectContents(null)).not.toThrow()
  })
})
