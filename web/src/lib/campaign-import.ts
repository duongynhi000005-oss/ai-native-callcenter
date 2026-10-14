import type { CampaignContactRow, CampaignImportResult } from './campaigns'
import { IMPORT_CHUNK_SIZE } from './campaigns'
import { toCsv } from './csv'
import type { Table } from './table'

/**
 * Turning a parsed list into import requests and the answers back into one
 * report. The parsing is in csv.ts (text files) and xlsx.ts (workbooks); this
 * file decides what each column means, which rows are sent, and how the
 * per-chunk results are put back together against the lines of the file the
 * operator knows.
 */

export type ColumnRole =
  | { kind: 'number' }
  | { kind: 'timeZone' }
  | { kind: 'slot'; name: string }
  | { kind: 'ignore' }

const NUMBER_HEADERS = /^(number|phone|phone.?number|mobile|msisdn|tel|telephone|cell|手机|手机号|手机号码|电话|电话号码|号码)$/i
const TIME_ZONE_HEADERS = /^(time.?zone|tz|timezone|时区)$/i

/**
 * A first guess at what each column is: the first column that reads like a
 * number is the number, one that reads like a zone is the zone, and every
 * other column is ignored until the operator makes it a flow field.
 */
export function guessRoles(header: string[]): ColumnRole[] {
  const numberAt = header.findIndex((h) => NUMBER_HEADERS.test(h.trim()))
  const zoneAt = header.findIndex((h) => TIME_ZONE_HEADERS.test(h.trim()))
  // With no header that reads like a number, the first column is the best bet.
  const chosenNumber = numberAt >= 0 ? numberAt : 0
  return header.map((_h, i): ColumnRole => {
    if (i === chosenNumber) return { kind: 'number' }
    if (i === zoneAt) return { kind: 'timeZone' }
    // Every other column starts ignored: a field the flow does not read makes
    // the server refuse the whole row, so a column is sent only when the
    // operator opts it in.
    return { kind: 'ignore' }
  })
}

export type RoleProblem =
  | 'NO_NUMBER' | 'MANY_NUMBERS' | 'MANY_TIME_ZONES' | 'EMPTY_SLOT_NAME' | 'DUPLICATE_SLOT'

/** What stops a mapping from being used, or undefined when it can be. */
export function mappingProblem(roles: ColumnRole[]): RoleProblem | undefined {
  const count = (kind: ColumnRole['kind']) => roles.filter((r) => r.kind === kind).length
  if (count('number') === 0) return 'NO_NUMBER'
  if (count('number') > 1) return 'MANY_NUMBERS'
  if (count('timeZone') > 1) return 'MANY_TIME_ZONES'
  const names = roles.flatMap((r) => (r.kind === 'slot' ? [r.name.trim()] : []))
  if (names.some((n) => n === '')) return 'EMPTY_SLOT_NAME'
  if (new Set(names).size !== names.length) return 'DUPLICATE_SLOT'
  return undefined
}

/** One record of the file, ready to send or already refused here. */
export interface ImportRecord {
  line: number
  cells: string[]
  /** Absent when the record never leaves the browser. */
  row?: CampaignContactRow
  localCode?: 'ROW_SHAPE'
  localDetail?: string
}

/**
 * Builds the rows to send. A record whose width differs from the header is not
 * guessed at: which cell is missing is unknowable, so it stays here and is
 * reported as rejected with the line it came from.
 */
export function buildRecords(table: Table, roles: ColumnRole[]): ImportRecord[] {
  const width = table.header.length
  const numberAt = roles.findIndex((r) => r.kind === 'number')
  const zoneAt = roles.findIndex((r) => r.kind === 'timeZone')
  // Without a number column nothing can be built; the dialog says so instead.
  if (numberAt < 0) return []
  const slots = roles.flatMap((r, index) => (r.kind === 'slot' ? [{ index, name: r.name.trim() }] : []))

  return table.rows.map((record): ImportRecord => {
    if (record.cells.length !== width) {
      return {
        line: record.line,
        cells: record.cells,
        localCode: 'ROW_SHAPE',
        localDetail: `${record.cells.length}/${width}`,
      }
    }
    const row: CampaignContactRow = { number: record.cells[numberAt].trim() }
    if (zoneAt >= 0 && record.cells[zoneAt].trim() !== '') row.timeZone = record.cells[zoneAt].trim()
    const values: Record<string, string> = {}
    for (const slot of slots) {
      const value = record.cells[slot.index].trim()
      if (value !== '') values[slot.name] = value
    }
    if (Object.keys(values).length > 0) row.slots = values
    return { line: record.line, cells: record.cells, row }
  })
}

export type EntryStatus = 'IMPORTED' | 'DUPLICATE' | 'REJECTED'

/** What happened to one record of the file. */
export interface ImportEntry {
  line: number
  cells: string[]
  status: EntryStatus
  code?: string
  detail?: string
  number?: string
}

export interface ImportSummary {
  imported: number
  duplicates: number
  rejected: number
  entries: ImportEntry[]
  /** Set when a request failed outright: the records from this index on were not sent. */
  failure?: { error: unknown; firstUnsent: number }
}

export const emptySummary = (): ImportSummary => ({
  imported: 0, duplicates: 0, rejected: 0, entries: [],
})

/** Adds a later run's report to an earlier one (a retry after a failed chunk). */
export function mergeSummaries(first: ImportSummary, next: ImportSummary): ImportSummary {
  return {
    imported: first.imported + next.imported,
    duplicates: first.duplicates + next.duplicates,
    rejected: first.rejected + next.rejected,
    entries: [...first.entries, ...next.entries],
    failure: next.failure,
  }
}

export type SendChunk = (rows: CampaignContactRow[]) => Promise<CampaignImportResult>

/**
 * Sends the records in requests of at most `chunkSize` rows and folds every
 * answer into one report, keyed by file line.
 *
 * Records refused here (wrong width) are never sent and count as rejected, in
 * file order with the rest. The server answers one result per request row in
 * request order, so a result is matched to its record by position within the
 * chunk, not by number. If a request fails outright the run stops and says
 * where: nothing after that point was sent, and re-sending what was is safe
 * because a number already on the campaign is answered DUPLICATE.
 */
export async function runImport(
  records: ImportRecord[],
  send: SendChunk,
  options: { chunkSize?: number; onProgress?: (done: number, total: number) => void } = {},
): Promise<ImportSummary> {
  const chunkSize = Math.min(options.chunkSize ?? IMPORT_CHUNK_SIZE, IMPORT_CHUNK_SIZE)
  const summary = emptySummary()
  const total = records.length

  for (let start = 0; start < total; start += chunkSize) {
    const slice = records.slice(start, start + chunkSize)
    const sendable = slice.filter((r) => r.row !== undefined)

    let result: CampaignImportResult | undefined
    if (sendable.length > 0) {
      try {
        // One request at a time, in file order: the report is keyed by position,
        // and a failure must leave a clean point to resume from.
        // eslint-disable-next-line no-await-in-loop
        result = await send(sendable.map((r) => r.row as CampaignContactRow))
      } catch (error) {
        summary.failure = { error, firstUnsent: start }
        return summary
      }
    }

    const byIndex = new Map(result?.rows.map((r) => [r.index, r]))
    let sentIndex = 0
    for (const record of slice) {
      if (!record.row) {
        summary.rejected++
        summary.entries.push({
          line: record.line, cells: record.cells, status: 'REJECTED',
          code: record.localCode, detail: record.localDetail,
        })
        continue
      }
      const answer = byIndex.get(sentIndex++)
      const status: EntryStatus = answer?.status ?? 'REJECTED'
      if (status === 'IMPORTED') summary.imported++
      else if (status === 'DUPLICATE') summary.duplicates++
      else summary.rejected++
      summary.entries.push({
        line: record.line, cells: record.cells, status,
        code: answer?.code, detail: answer?.detail, number: answer?.number,
      })
    }
    options.onProgress?.(Math.min(start + chunkSize, total), total)
  }
  return summary
}

/**
 * The refused rows as a CSV the operator can fix and send again. The three
 * leading columns say where each row was and why it was refused; they come
 * first because a row of the wrong width cannot be aligned to the end.
 */
export function rejectedCsv(header: string[], entries: ImportEntry[]): string {
  const rejected = entries.filter((e) => e.status === 'REJECTED')
  return toCsv([
    ['line', 'code', 'detail', ...header],
    ...rejected.map((e) => [String(e.line), e.code ?? '', e.detail ?? ''].concat(e.cells)),
  ])
}
