/**
 * A small RFC 4180 reader and writer.
 *
 * Contact lists arrive from spreadsheets and CRMs, so the reader is tolerant
 * where the formats differ (BOM, CRLF or LF, a comma, semicolon, tab or pipe as
 * the delimiter, blank lines, rows of all-empty cells) and strict where a
 * guess would corrupt data (quotes are only recognised at the start of a
 * field, and a record whose width differs from the header is kept as it is so
 * the caller can refuse it, never padded or cut).
 *
 * It is a single pass over the string with no per-character allocation, so a
 * list of tens of thousands of rows parses in a few milliseconds.
 */

export interface CsvRecord {
  /** 1-based line of the file this record starts on, for messages. */
  line: number
  cells: string[]
}

export interface CsvTable {
  delimiter: string
  /** The first non-blank record. */
  header: string[]
  rows: CsvRecord[]
  /** True when the file ended inside a quoted field. */
  hasUnterminatedQuote: boolean
}

const DELIMITERS = [',', ';', '\t', '|']

/** Picks the delimiter that splits the first record into the most cells. */
export function sniffDelimiter(text: string): string {
  let inQuotes = false
  const counts = new Map<string, number>(DELIMITERS.map((d) => [d, 0]))
  for (let i = 0; i < text.length; i++) {
    const ch = text[i]
    if (ch === '"') inQuotes = !inQuotes
    else if (!inQuotes && (ch === '\n' || ch === '\r')) {
      if ([...counts.values()].some((n) => n > 0)) break
    } else if (!inQuotes && counts.has(ch)) counts.set(ch, (counts.get(ch) ?? 0) + 1)
  }
  let best = ','
  let bestCount = 0
  for (const delimiter of DELIMITERS) {
    const n = counts.get(delimiter) ?? 0
    if (n > bestCount) {
      best = delimiter
      bestCount = n
    }
  }
  return best
}

/** Reads every record of a CSV text. Blank records are dropped. */
export function parseCsv(input: string, delimiterHint?: string): CsvTable {
  const text = input.charCodeAt(0) === 0xfeff ? input.slice(1) : input
  const delimiter = delimiterHint ?? sniffDelimiter(text)

  const records: CsvRecord[] = []
  let cells: string[] = []
  let field = ''
  // Where the unquoted run being read began, so plain text is copied with one
  // slice rather than a character at a time.
  let runStart = 0
  let line = 1
  let recordLine = 1
  let hasQuotedCell = false
  let hasUnterminatedQuote = false

  const endField = () => {
    cells.push(field)
    field = ''
  }
  const endRecord = () => {
    endField()
    const isBlank = !hasQuotedCell && cells.every((c) => c.trim() === '')
    if (!isBlank) records.push({ line: recordLine, cells })
    cells = []
    hasQuotedCell = false
  }

  let i = 0
  const n = text.length
  while (i < n) {
    // Start of a field.
    if (text[i] === '"') {
      hasQuotedCell = true
      i++
      let closed = false
      while (i < n) {
        const q = text.indexOf('"', i)
        if (q === -1) {
          field += text.slice(i)
          for (let k = i; k < n; k++) if (text[k] === '\n') line++
          i = n
          break
        }
        field += text.slice(i, q)
        for (let k = i; k < q; k++) if (text[k] === '\n') line++
        if (text[q + 1] === '"') {
          field += '"'
          i = q + 2
        } else {
          i = q + 1
          closed = true
          break
        }
      }
      if (!closed) hasUnterminatedQuote = true
      // Anything between the closing quote and the delimiter is kept as text.
      runStart = i
      while (i < n && text[i] !== delimiter && text[i] !== '\n' && text[i] !== '\r') i++
      field += text.slice(runStart, i)
    } else {
      runStart = i
      while (i < n && text[i] !== delimiter && text[i] !== '\n' && text[i] !== '\r') i++
      field += text.slice(runStart, i)
    }

    if (i >= n) {
      endRecord()
      break
    }
    const ch = text[i]
    if (ch === delimiter) {
      endField()
      i++
      if (i >= n) {
        // A trailing delimiter ends with an empty cell.
        endRecord()
      }
      continue
    }
    // A line break: CRLF, CR or LF.
    if (ch === '\r' && text[i + 1] === '\n') i++
    i++
    line++
    endRecord()
    recordLine = line
  }

  const header = records.shift()?.cells ?? []
  return { delimiter, header: header.map((c) => c.trim()), rows: records, hasUnterminatedQuote }
}

/** Quotes a cell when it needs it. */
function quote(cell: string, delimiter: string): string {
  if (cell === '' || (!/["\r\n]/.test(cell) && !cell.includes(delimiter) && cell === cell.trim())) {
    return cell
  }
  return `"${cell.replaceAll('"', '""')}"`
}

/**
 * Writes rows as CSV with CRLF line ends. A BOM leads the file so a
 * spreadsheet opens Chinese text as UTF-8 instead of guessing a code page.
 */
export function toCsv(rows: string[][], options: { delimiter?: string; bom?: boolean } = {}): string {
  const delimiter = options.delimiter ?? ','
  const body = rows.map((row) => row.map((cell) => quote(cell, delimiter)).join(delimiter)).join('\r\n')
  return (options.bom === false ? '' : '﻿') + body + '\r\n'
}
