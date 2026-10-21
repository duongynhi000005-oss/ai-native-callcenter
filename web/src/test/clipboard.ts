import { afterEach, vi } from 'vitest'

/**
 * The clipboard a test runs against, chosen on purpose.
 *
 * `userEvent.setup()` installs a clipboard of its own, so a suite that leaves
 * it in place always runs as if the page were in a secure context, and cannot
 * see what happens over plain HTTP, where `navigator.clipboard` is undefined
 * (#290). These replace it explicitly, after the render (which is where the
 * harness calls `userEvent.setup()`), and put it back after each test.
 */

let saved: { descriptor: PropertyDescriptor | undefined } | null = null

function defineClipboard(value: unknown) {
  if (saved === null) saved = { descriptor: Object.getOwnPropertyDescriptor(navigator, 'clipboard') }
  Object.defineProperty(navigator, 'clipboard', { value, configurable: true })
}

/** A page served over plain HTTP: there is no clipboard API at all. */
export function removeClipboard() {
  defineClipboard(undefined)
}

/** A secure context whose clipboard does what `writeText` says. */
export function stubClipboard(writeText: (value: string) => Promise<void>) {
  const spy = vi.fn(writeText)
  defineClipboard({ writeText: spy })
  return spy
}

/** A clipboard write the test settles by hand, to look before and after. */
export function pendingWrite() {
  let handlers: { resolve: () => void; reject: (reason: unknown) => void } | undefined
  const promise = new Promise<void>((resolve, reject) => {
    handlers = { resolve, reject }
  })
  return {
    write: () => promise,
    settle: () => handlers?.resolve(),
    refuse: (reason: unknown) => handlers?.reject(reason),
  }
}

/** A write the browser refused, as Chrome refuses it. */
export function notAllowed() {
  return new DOMException('Write permission denied.', 'NotAllowedError')
}

export interface CopyCommand {
  command: string
  /** What was selected when the command ran: what the browser would copy. */
  selected: string
  /** The element holding that selection when the command ran. */
  holder: Element | null
  /** Where that element sat in the page then. */
  parent: Element | null
}

/**
 * `document.execCommand`, which jsdom does not implement. `result` is what the
 * browser answers: true for a copy made, false for one refused, or a throw.
 */
export function stubExecCommand(result: boolean | 'throw') {
  const commands: CopyCommand[] = []
  Object.defineProperty(document, 'execCommand', {
    configurable: true,
    writable: true,
    value: vi.fn((command: string) => {
      const holder = document.activeElement
      const selected =
        holder instanceof HTMLTextAreaElement || holder instanceof HTMLInputElement
          ? holder.value.slice(holder.selectionStart ?? 0, holder.selectionEnd ?? 0)
          : String(window.getSelection() ?? '')
      commands.push({ command, selected, holder, parent: holder?.parentElement ?? null })
      if (result === 'throw') throw new DOMException('Not allowed.', 'SecurityError')
      return result
    }),
  })
  return commands
}

/** The text the reader would copy with Ctrl/Cmd+C right now. */
export function selectedText(): string {
  const holder = document.activeElement
  if (holder instanceof HTMLTextAreaElement || holder instanceof HTMLInputElement) {
    return holder.value.slice(holder.selectionStart ?? 0, holder.selectionEnd ?? 0)
  }
  return String(window.getSelection() ?? '')
}

/** Every text an element showed, from now until it is read. */
export function recordTexts(element: Element) {
  const seen = [element.textContent ?? '']
  const observer = new MutationObserver(() => seen.push(element.textContent ?? ''))
  observer.observe(element, { subtree: true, childList: true, characterData: true })
  return {
    seen,
    stop: () => observer.disconnect(),
  }
}

afterEach(() => {
  if (saved !== null) {
    if (saved.descriptor) Object.defineProperty(navigator, 'clipboard', saved.descriptor)
    else Reflect.deleteProperty(navigator, 'clipboard')
    saved = null
  }
  Reflect.deleteProperty(document, 'execCommand')
  window.getSelection()?.removeAllRanges()
})
