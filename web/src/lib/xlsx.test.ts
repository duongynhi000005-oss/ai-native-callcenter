import { readFileSync } from 'node:fs'
import path from 'node:path'
import { describe, expect, it } from 'vitest'

import { parseCsv } from './csv'
import type { Table } from './table'
import { expandScientific, readXlsxWorkbook } from './xlsx'

/**
 * The fixtures are real workbooks: `scripts/make-xlsx-fixtures.mjs` writes the
 * ZIP and its OOXML parts, and `openpyxl` reads them back, so what is under
 * test is the format rather than this repository's idea of it. They are read as
 * bytes rather than handed over as a `File`, so the test does not lean on
 * jsdom's `Blob`.
 */
/**
 * Reading a workbook is heavier than reading text: the reader is a chunk of its
 * own, transformed on first use, and under a full run that first transform has
 * taken over five seconds on this machine. The budget is for that machinery,
 * not for what the tests assert.
 */
const READING = 20_000

/** `it` for a test that reads a workbook. */
function itReads(title: string, body: () => Promise<void>) {
  it(title, body, READING)
}

function fixturePath(name: string): string {
  return path.join(import.meta.dirname, '..', 'test', 'fixtures', name)
}

function fixtureBytes(name: string): ArrayBuffer {
  const bytes = readFileSync(fixturePath(name))
  return bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) as ArrayBuffer
}

function fixtureText(name: string): string {
  return readFileSync(fixturePath(name), 'utf8')
}

async function sheetsOf(name: string) {
  return readXlsxWorkbook(fixtureBytes(name))
}

async function sheetOf(name: string): Promise<Table> {
  return (await sheetsOf(name))[0].table
}

describe('a workbook and the CSV of the same contacts', () => {
  itReads('read to exactly the same header and rows', async () => {
    const csv = parseCsv(fixtureText('contacts.csv'))
    // The numbers are numeric cells in the workbook and bare text in the CSV.
    expect(await sheetOf('contacts.xlsx')).toEqual({ header: csv.header, rows: csv.rows })
  })

  itReads('give the phone number as its digits, never as 1.38e10', async () => {
    const table = await sheetOf('contacts.xlsx')
    expect(table.rows.map((row) => row.cells[0])).toEqual([
      '13800138000',
      '13900139000',
      '13300133000',
    ])
  })

  itReads('name the sheet the workbook gives it', async () => {
    expect((await sheetsOf('contacts.xlsx')).map((sheet) => sheet.name)).toEqual(['联系人'])
  })
})

describe('the cells a CSV cannot express', () => {
  itReads('arrive as the value the spreadsheet holds', async () => {
    const table = await sheetOf('edge-cases.xlsx')
    expect(table.header).toEqual(['手机号', '姓名', '开通日期', '最后联系', '已确认', '行数'])
    expect(table.rows.map((row) => row.cells)).toEqual([
      // Text with a leading zero, a date, a date and time, a boolean, a formula.
      ['013800138000', '王伟', '2026-10-13', '2026-10-13 12:00:00', 'TRUE', '6'],
      // Stored in scientific notation by whoever wrote the file.
      ['13800138000', '李静', '2026-01-02', '2026-01-02 06:00:00', 'FALSE', '2'],
      // A number whose *display* format is `0.00E+00`; the digits win.
      ['13800138000', '陈涛', '2026-12-31', '2026-12-31 18:45:00', 'TRUE', '6'],
      // A merged range: the anchor carries the value, the rest are its shape.
      ['合并单元格：只有 A6 有值', '', '', '', '', ''],
    ])
  })

  itReads('keep the row numbers the spreadsheet shows', async () => {
    const table = await sheetOf('edge-cases.xlsx')
    // The blank line is dropped, as a blank line in a CSV is; the empty rows
    // after the merged one leave nothing behind; and the merged row is still
    // row 6, which is where the operator sees it.
    expect(table.rows.map((row) => row.line)).toEqual([2, 3, 4, 6])
  })
})

describe("a sheet whose first rows hold nothing", () => {
  itReads('keeps the row numbers the spreadsheet shows', async () => {
    const table = await sheetOf('offset.xlsx')
    // The header is row 3 of the sheet, not row 1. A spreadsheet writes no row
    // for the two empty ones above it, so their absence is what has to be
    // noticed, or every row in the report would be named two rows too early.
    expect(table.header).toEqual(['手机号', '姓名', '时区', '备注'])
    expect(table.rows.map((row) => row.line)).toEqual([4, 5, 6])
    expect(table.rows.map((row) => row.cells[0])).toEqual([
      '13800138000',
      '13900139000',
      '13300133000',
    ])
  })
})

describe('a workbook of several sheets', () => {
  itReads('is returned whole, in the workbook\'s own order', async () => {
    const sheets = await sheetsOf('two-sheets.xlsx')
    expect(sheets.map((sheet) => sheet.name)).toEqual(['联系人', '备用名单'])
    expect(sheets[1].table.header).toEqual(['手机号', '姓名'])
    expect(sheets[1].table.rows.map((row) => row.cells)).toEqual([['13500135000', '赵敏']])
  })
})

describe('a file that is not a workbook', () => {
  itReads('is refused rather than half-read', async () => {
    await expect(readXlsxWorkbook(fixtureBytes('broken.xlsx'))).rejects.toThrow()
  })
})

describe('scientific notation out of a cell', () => {
  it('is put back into place digit by digit', () => {
    expect(expandScientific('1.38E+10')).toBe('13800000000')
    expect(expandScientific('1.3800138E+10')).toBe('13800138000')
    expect(expandScientific('5E+3')).toBe('5000')
    expect(expandScientific('1.20E+1')).toBe('12')
    expect(expandScientific('1.38E-3')).toBe('0.00138')
    expect(expandScientific('-2.5E+2')).toBe('-250')
  })

  it('leaves anything that is not one alone', () => {
    for (const text of ['13800138000', '013800138000', '1.38', '1.38E', 'E+10', '1.38E+10 W', '']) {
      expect(expandScientific(text)).toBe(text)
    }
  })
})
