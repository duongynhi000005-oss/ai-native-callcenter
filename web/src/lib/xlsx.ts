import type { Table, TableRecord } from './table'

/**
 * An `.xlsx` workbook read in the browser, as the same row model the CSV
 * reader produces: every sheet in the file's own order, each with its header
 * and the records under it.
 *
 * The reading is `read-excel-file` (MIT, read-only), imported lazily so a
 * build that never opens a workbook does not carry it.
 *
 * What a cell becomes is the cell's own value, never a float that has lost it:
 *
 * - a cell holding text is that text, so a number typed with a leading zero
 *   keeps it;
 * - a numeric cell is the digits the file stores (`parseNumber` hands back the
 *   cell's text untouched), so an 11-digit number stays a number and never
 *   becomes `1.38e10`; digits a writer stored in scientific notation are
 *   expanded without going through a float either;
 * - a formula cell is its cached result, which is what the spreadsheet last
 *   displayed;
 * - a date cell is the date it holds, as `yyyy-mm-dd`, with the time appended
 *   when the value has one;
 * - a boolean is `TRUE` or `FALSE`, an empty cell is `''`.
 *
 * What is *not* applied is the cell's number *format*: a number an operator
 * padded on screen (store `1380013800`, display `01380013800`) arrives as
 * `1380013800`. `read-excel-file` reports values, and the styles that turn a
 * value into a display string are not part of what it hands back; carrying
 * them would mean reading the sheet's XML a second time. Numbers are what a
 * dialler calls, and a number Excel was showing in scientific notation is
 * exactly the case the format must not win.
 */

/**
 * A numeric cell's digits, marked as numeric.
 *
 * `parseNumber` is what turns the library's parsed float back into the text
 * the file stores. It answers with an object rather than the bare string so
 * that a *text* cell reading `1.38E+10` — an operator's own characters — is
 * never mistaken for a number that has to be put back into place.
 */
interface NumericText {
  numericText: string
}

/** The rows of one sheet, with the cells as the library returned them. */
type SheetRow = readonly (string | NumericText | boolean | Date | null)[]

/** A sheet of a workbook, ready for the mapping step. */
export interface XlsxSheet {
  name: string
  table: Table
}

/**
 * Reads every sheet of an `.xlsx` file. Throws when the file is not one (a
 * corrupt archive, a legacy `.xls`, a CSV renamed) or holds no worksheet — the
 * caller says so.
 */
export async function readXlsxWorkbook(file: Blob | ArrayBuffer): Promise<XlsxSheet[]> {
  // Loaded here, not at the top of the module: the dialog is on a route the
  // SPA needs anyway, and this library is only needed once a workbook is
  // chosen. Vite puts it in a chunk of its own.
  const { default: readXlsxFile } = await import('read-excel-file/browser')
  const sheets = await readXlsxFile<NumericText>(file, {
    // The pass-through that keeps the cell's own text: the library would
    // otherwise parse the value into a Number, and `Number` then decides how
    // many digits a phone number has.
    parseNumber: (text) => ({ numericText: text }),
  })
  // A workbook with no worksheet is not one to map columns against, and the
  // caller has nothing to show for it — better refused than opened empty.
  if (sheets.length === 0) throw new Error('the workbook has no sheets')
  return sheets.map((sheet) => ({ name: sheet.sheet, table: tableOf(sheet.data as SheetRow[]) }))
}

/** One sheet's rows as a table: the first non-blank row is the header. */
function tableOf(data: SheetRow[]): Table {
  // The library trims empty rows and columns at the edges and fills the gaps
  // it repaired with empty cells, so row `i` of `data` is row `i + 1` of the
  // sheet: the number the operator sees in the spreadsheet's own gutter.
  const records: TableRecord[] = data.map((cells, index) => ({
    line: index + 1,
    cells: cells.map(cellText),
  }))
  // A row of nothing is not a contact with an empty number, it is the blank
  // line a CSV reader drops. Dropped here too, before the header is taken.
  const kept = records.filter((record) => record.cells.some((cell) => cell.trim() !== ''))
  const header = kept.shift()?.cells ?? []
  return { header: header.map((cell) => cell.trim()), rows: kept }
}

/** The cell as the operator reads it, as text. */
function cellText(value: SheetRow[number]): string {
  if (value === null || value === undefined) return ''
  if (typeof value === 'string') return value
  if (typeof value === 'boolean') return value ? 'TRUE' : 'FALSE'
  if (value instanceof Date) return dateText(value)
  return expandScientific(value.numericText)
}

/** The digits a writer stored as `1.38E+10`, moved back into place: `13800000000`. */
export function expandScientific(text: string): string {
  const match = /^([+-]?)(\d+)(?:\.(\d*))?[eE]([+-]?\d+)$/.exec(text.trim())
  if (!match) return text
  const [, sign, whole, fraction = '', exponent] = match
  const digits = whole + fraction
  // Where the decimal point of the number sits within `digits`.
  const point = whole.length + Number(exponent)
  let plain: string
  if (point <= 0) plain = `0.${'0'.repeat(-point)}${digits}`
  else if (point >= digits.length) plain = digits + '0'.repeat(point - digits.length)
  else plain = `${digits.slice(0, point)}.${digits.slice(point)}`
  // A fraction of nothing (`1.20E+1` is 12) is not how a number is written.
  if (plain.includes('.')) plain = plain.replace(/0+$/, '').replace(/\.$/, '')
  return sign + plain
}

/**
 * A date cell as the date it holds. The library reads the spreadsheet's serial
 * number against the workbook's own epoch and hands back the instant, so the
 * calendar date is the same wherever the operator's machine is.
 */
function dateText(date: Date): string {
  const iso = date.toISOString()
  return iso.endsWith('T00:00:00.000Z') ? iso.slice(0, 10) : iso.slice(0, 19).replace('T', ' ')
}
