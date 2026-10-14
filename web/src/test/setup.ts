import '@testing-library/jest-dom/vitest'
import { configure } from '@testing-library/react'

// The default 1000ms is the budget for one findBy* to settle. On a cold CI
// runner the first render of the heaviest route file is also the first time
// its module graph is evaluated, and that alone has taken longer than the
// budget (2026-09-10: the cockpit's first test failed at 1082ms on a run
// whose environment setup took eight seconds). A wider budget changes
// nothing a test asserts, only how long it is willing to wait for it.
configure({ asyncUtilTimeout: 5000 })

// jsdom's `Blob` has no `arrayBuffer()` — it implements `FileReader` and not
// the promise-based readers that came later. The workbook reader asks a chosen
// `File` for its bytes that way, so without this a browser-only code path
// cannot be tested at all: the importer would look as though it refused every
// workbook. Reading through `FileReader` gives the same bytes in the same
// order, and only stands in where the browser already has the method.
function readAsArrayBuffer(blob: Blob): Promise<ArrayBuffer> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.addEventListener('load', () => resolve(reader.result as ArrayBuffer))
    reader.addEventListener('error', () => reject(reader.error))
    reader.readAsArrayBuffer(blob)
  })
}

if (typeof Blob !== 'undefined' && typeof Blob.prototype.arrayBuffer !== 'function') {
  Blob.prototype.arrayBuffer = function arrayBuffer(this: Blob) {
    return readAsArrayBuffer(this)
  }
}
