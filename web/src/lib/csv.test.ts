import { describe, expect, it } from 'vitest'

import { parseCsv, sniffDelimiter, toCsv } from './csv'

describe('the CSV reader', () => {
  it('reads a plain file', () => {
    const table = parseCsv('number,name\n13800138000,Wei\n13900139000,Li\n')
    expect(table.header).toEqual(['number', 'name'])
    expect(table.rows.map((r) => r.cells)).toEqual([
      ['13800138000', 'Wei'],
      ['13900139000', 'Li'],
    ])
    expect(table.delimiter).toBe(',')
  })

  it('drops a UTF-8 byte order mark so it never sticks to the first header', () => {
    const table = parseCsv('﻿number,name\n1,a\n')
    expect(table.header).toEqual(['number', 'name'])
  })

  it('reads the same file with and without the mark', () => {
    const body = '手机号,姓名\n13800138000,王伟\n'
    expect(parseCsv('﻿' + body)).toEqual(parseCsv(body))
  })

  it('reads CRLF, bare CR and LF line ends alike', () => {
    for (const eol of ['\r\n', '\n', '\r']) {
      const table = parseCsv(['n,a', '1,x', '2,y'].join(eol) + eol)
      expect(table.rows.map((r) => r.cells)).toEqual([['1', 'x'], ['2', 'y']])
    }
  })

  it('keeps Chinese headers and values, and emoji, intact', () => {
    const table = parseCsv('手机号,姓名,备注\n13800138000,王伟,想了解套餐 😀\n')
    expect(table.header).toEqual(['手机号', '姓名', '备注'])
    expect(table.rows[0].cells).toEqual(['13800138000', '王伟', '想了解套餐 😀'])
  })

  it('reads quoted fields with commas, doubled quotes and embedded newlines', () => {
    const table = parseCsv(
      'number,note\r\n1,"a, b"\r\n2,"she said ""hi"""\r\n3,"line one\r\nline two"\r\n4,plain\r\n',
    )
    expect(table.rows.map((r) => r.cells)).toEqual([
      ['1', 'a, b'],
      ['2', 'she said "hi"'],
      ['3', 'line one\r\nline two'],
      ['4', 'plain'],
    ])
  })

  it('numbers records by the line of the file they start on, newlines in quotes included', () => {
    const table = parseCsv('n,note\n1,"a\nb\nc"\n2,x\n')
    expect(table.rows.map((r) => r.line)).toEqual([2, 5])
  })

  it('skips blank lines and rows of nothing but delimiters', () => {
    const table = parseCsv('n,a\n\n1,x\n  \n,\n2,y\n\n')
    expect(table.rows.map((r) => r.cells)).toEqual([['1', 'x'], ['2', 'y']])
  })

  it('keeps a quoted empty value as a row', () => {
    const table = parseCsv('n,a\n"",""\n')
    expect(table.rows).toHaveLength(1)
  })

  it('keeps a row of the wrong width exactly as it is', () => {
    const table = parseCsv('n,a,b\n1,x\n2,y,z,extra\n3,p,q\n')
    expect(table.rows.map((r) => r.cells.length)).toEqual([2, 4, 3])
  })

  it('reads a trailing delimiter as an empty last cell', () => {
    expect(parseCsv('a,b\n1,').rows[0].cells).toEqual(['1', ''])
  })

  it('reads a last record that has no line end', () => {
    expect(parseCsv('a,b\n1,2').rows[0].cells).toEqual(['1', '2'])
  })

  it('flags a quote that is never closed', () => {
    const table = parseCsv('a,b\n1,"oops\n2,x\n')
    expect(table.hasUnterminatedQuote).toBe(true)
  })

  it('treats a quote in the middle of a value as text', () => {
    expect(parseCsv('a\n5" pipe\n').rows[0].cells).toEqual(['5" pipe'])
  })

  it('handles an empty file', () => {
    const table = parseCsv('')
    expect(table.header).toEqual([])
    expect(table.rows).toEqual([])
  })

  it('reads several thousand rows quickly and loses none', () => {
    const lines = ['number,name,note']
    for (let i = 0; i < 20000; i++) {
      lines.push(`138${String(i).padStart(8, '0')},客户${i},"备注, 第${i}行\n续"`)
    }
    const started = performance.now()
    const table = parseCsv(lines.join('\r\n'))
    const elapsed = performance.now() - started
    expect(table.rows).toHaveLength(20000)
    expect(table.rows[19999].cells[1]).toBe('客户19999')
    expect(table.rows[19999].cells[2]).toBe('备注, 第19999行\n续')
    // Far under what would freeze a page; the bound is loose on purpose.
    expect(elapsed).toBeLessThan(1500)
  })
})

describe('delimiter detection', () => {
  it('finds a semicolon, a tab and a pipe', () => {
    expect(sniffDelimiter('a;b;c\n1;2;3')).toBe(';')
    expect(sniffDelimiter('a\tb\tc\n1\t2\t3')).toBe('\t')
    expect(sniffDelimiter('a|b|c\n1|2|3')).toBe('|')
  })

  it('ignores delimiters inside quotes on the header line', () => {
    expect(sniffDelimiter('"a,b,c";d\n1;2')).toBe(';')
  })

  it('falls back to a comma', () => {
    expect(sniffDelimiter('single\nvalue')).toBe(',')
  })

  it('parses a semicolon file with Chinese headers', () => {
    const table = parseCsv('号码;时区;城市\n13800138000;Asia/Shanghai;上海\n')
    expect(table.delimiter).toBe(';')
    expect(table.rows[0].cells).toEqual(['13800138000', 'Asia/Shanghai', '上海'])
  })
})

describe('the CSV writer', () => {
  it('round-trips awkward values through the reader', () => {
    const rows = [
      ['line', 'note'],
      ['1', 'plain'],
      ['2', 'a, b'],
      ['3', 'she said "hi"'],
      ['4', 'two\r\nlines'],
      ['5', ' padded '],
      ['6', '中文，逗号'],
      ['7', ''],
    ]
    const table = parseCsv(toCsv(rows))
    expect([table.header, ...table.rows.map((r) => r.cells)]).toEqual(rows)
  })

  it('leads with a byte order mark so a spreadsheet reads UTF-8', () => {
    expect(toCsv([['a']]).charCodeAt(0)).toBe(0xfeff)
    expect(toCsv([['a']], { bom: false })).toBe('a\r\n')
  })
})
