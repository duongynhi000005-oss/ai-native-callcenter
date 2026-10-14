/**
 * What a parsed list looks like, whatever it arrived in.
 *
 * A CSV file and a sheet of an Excel workbook both end up as this: a header and
 * the records under it. Everything downstream — guessing what each column is,
 * building the rows to send, the report, the rejected-rows download — works on
 * this shape and never asks where it came from.
 */

export interface TableRecord {
  /**
   * Where the record came from, for the messages the operator reads: the line
   * of a text file, the row number of a spreadsheet.
   */
  line: number
  cells: string[]
}

export interface Table {
  /** The first non-blank record, trimmed. */
  header: string[]
  rows: TableRecord[]
}
