import fs from 'node:fs/promises'
import path from 'node:path'
import { describe, expect, it } from 'vitest'

/**
 * The served HTML keeps the web-sip-phone widget off pages without a phone.
 *
 * The extension (1.1.1 and later) reads this declaration once, when its
 * content script is injected, and only from the HTML the server sent: a tag
 * added by script is ignored. So it has to live in `index.html`, and nothing
 * the SPA renders can stand in for it. Without it, an administrator and the
 * sign-in page get a "Not connected" phone card (docs/web-sip-phone-protocol.md
 * §2, R9).
 */

const INDEX_HTML = path.resolve(import.meta.dirname, '../../index.html')

describe('index.html', () => {
  it('declares page-controlled widget visibility for web-sip-phone', async () => {
    const html = await fs.readFile(INDEX_HTML, 'utf8')
    const doc = new DOMParser().parseFromString(html, 'text/html')
    // The extension lets the first tag decide, and so does this test.
    const declaration = doc.head.querySelector('meta[name="web-sip-phone"]')
    expect(declaration?.getAttribute('content')).toBe('widget=page')
  })
})
