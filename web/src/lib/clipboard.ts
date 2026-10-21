/**
 * Copying a value for the reader, and saying truthfully whether it happened.
 *
 * `navigator.clipboard` exists only in a secure context (HTTPS, or localhost).
 * This product is served over plain HTTP on a LAN address by default, so there
 * it is `undefined`, and the copy falls back to selecting the text in a hidden
 * textarea and running the `copy` command. That command is deprecated, but it
 * is the one copy a page served over HTTP can still make in Chrome.
 *
 * Either path can be refused (a blocked permission, an unfocused document), so
 * the answer is a boolean the caller must act on: "Copied" only after a write
 * that succeeded, and otherwise a message and the value selected, so that
 * Ctrl/Cmd+C works at once (#290).
 */
export async function copyText(value: string): Promise<boolean> {
  const clipboard = typeof navigator === 'undefined' ? undefined : navigator.clipboard
  if (typeof clipboard?.writeText === 'function') {
    try {
      await clipboard.writeText(value)
      return true
    } catch {
      return false
    }
  }
  // Reached without an await, so the command runs inside the click that asked
  // for it: the browser refuses a copy made outside a user gesture.
  return copyBySelection(value)
}

function copyBySelection(value: string): boolean {
  if (typeof document === 'undefined' || !document.body) return false
  const focused = document.activeElement instanceof HTMLElement ? document.activeElement : null
  // Placed beside the element that has focus rather than at the end of the
  // body: inside a modal, a focus trap pulls focus straight back from anything
  // outside the dialog, and the selection would go with it.
  const host = focused && focused !== document.body ? (focused.parentElement ?? document.body) : document.body

  const area = document.createElement('textarea')
  area.value = value
  area.readOnly = true
  area.tabIndex = -1
  area.setAttribute('aria-hidden', 'true')
  Object.assign(area.style, {
    position: 'fixed',
    top: '0',
    left: '0',
    width: '1px',
    height: '1px',
    padding: '0',
    border: '0',
    opacity: '0',
    pointerEvents: 'none',
  })
  host.appendChild(area)
  try {
    area.focus({ preventScroll: true })
    area.select()
    area.setSelectionRange(0, value.length)
    return document.execCommand('copy') === true
  } catch {
    return false
  } finally {
    area.remove()
    focused?.focus({ preventScroll: true })
  }
}

/**
 * Selects an element's text, so a copy the browser refused can be finished
 * with the keyboard.
 */
export function selectContents(element: Element | null | undefined): void {
  if (!element) return
  if (element instanceof HTMLInputElement || element instanceof HTMLTextAreaElement) {
    element.focus({ preventScroll: true })
    element.select()
    return
  }
  const selection = typeof window === 'undefined' ? null : window.getSelection()
  if (!selection) return
  const range = document.createRange()
  range.selectNodeContents(element)
  selection.removeAllRanges()
  selection.addRange(range)
}
