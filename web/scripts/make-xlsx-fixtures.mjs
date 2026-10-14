// Writes the `.xlsx` fixtures the workbook tests read, and the CSV twin they
// are compared against. Run it with:
//
//     node scripts/make-xlsx-fixtures.mjs
//
// The fixtures are committed, so this only has to run when one of them
// changes. They are built here rather than with a spreadsheet library because
// the point of them is to be *real* files — a hand-written ZIP whose parts are
// the OOXML a spreadsheet writes — so the reader is tested against the format
// and not against a library's idea of it. `openpyxl` is used as an independent
// check that the bytes are genuine (see the end of this file).
//
// Nothing here is imported by the app or by a test: the tests read the files.

import { deflateRawSync } from 'node:zlib'
import { mkdirSync, writeFileSync } from 'node:fs'
import path from 'node:path'

// ---------------------------------------------------------------------------
// A minimal ZIP writer. OOXML is a ZIP of XML parts, and a spreadsheet reads
// nothing else, so this is the whole container: local header, data, central
// directory, end record.

const CRC_TABLE = (() => {
  const table = new Int32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    table[n] = c
  }
  return table
})()

function crc32(buffer) {
  let crc = -1
  for (let i = 0; i < buffer.length; i++) crc = (crc >>> 8) ^ CRC_TABLE[(crc ^ buffer[i]) & 0xff]
  return (crc ^ -1) >>> 0
}

// A fixed date, so the same input always produces the same bytes and a
// regenerated fixture shows up as no change in `git status`.
const DOS_TIME = 0
const DOS_DATE = ((2026 - 1980) << 9) | (1 << 5) | 1

/** Zips `entries` (`{ name, data }`) into one archive. */
function zip(entries) {
  const parts = []
  const central = []
  let offset = 0

  for (const entry of entries) {
    const name = Buffer.from(entry.name, 'utf8')
    const raw = Buffer.isBuffer(entry.data) ? entry.data : Buffer.from(entry.data, 'utf8')
    const deflated = deflateRawSync(raw, { level: 9 })
    // Stored, not deflated, when compression makes it bigger — the same rule a
    // real archiver follows.
    const useDeflate = deflated.length < raw.length
    const body = useDeflate ? deflated : raw
    const method = useDeflate ? 8 : 0
    const crc = crc32(raw)

    const local = Buffer.alloc(30)
    local.writeUInt32LE(0x04034b50, 0)
    local.writeUInt16LE(20, 4) // version needed
    local.writeUInt16LE(0x0800, 6) // UTF-8 names
    local.writeUInt16LE(method, 8)
    local.writeUInt16LE(DOS_TIME, 10)
    local.writeUInt16LE(DOS_DATE, 12)
    local.writeUInt32LE(crc, 14)
    local.writeUInt32LE(body.length, 18)
    local.writeUInt32LE(raw.length, 22)
    local.writeUInt16LE(name.length, 26)
    local.writeUInt16LE(0, 28)
    parts.push(local, name, body)

    const dir = Buffer.alloc(46)
    dir.writeUInt32LE(0x02014b50, 0)
    dir.writeUInt16LE(20, 4) // version made by
    dir.writeUInt16LE(20, 6) // version needed
    dir.writeUInt16LE(0x0800, 8)
    dir.writeUInt16LE(method, 10)
    dir.writeUInt16LE(DOS_TIME, 12)
    dir.writeUInt16LE(DOS_DATE, 14)
    dir.writeUInt32LE(crc, 16)
    dir.writeUInt32LE(body.length, 20)
    dir.writeUInt32LE(raw.length, 24)
    dir.writeUInt16LE(name.length, 28)
    // extra, comment, disk, internal attributes, external attributes
    dir.writeUInt32LE(0, 36)
    dir.writeUInt32LE(offset, 42)
    central.push(dir, name)

    offset += local.length + name.length + body.length
  }

  const dirStart = offset
  const dirSize = central.reduce((n, part) => n + part.length, 0)

  const end = Buffer.alloc(22)
  end.writeUInt32LE(0x06054b50, 0)
  end.writeUInt16LE(entries.length, 8)
  end.writeUInt16LE(entries.length, 10)
  end.writeUInt32LE(dirSize, 12)
  end.writeUInt32LE(dirStart, 16)

  return Buffer.concat([...parts, ...central, end])
}

// ---------------------------------------------------------------------------
// The OOXML parts.

const XML_HEAD = '<?xml version="1.0" encoding="UTF-8" standalone="yes"?>\n'
const NS_MAIN = 'http://schemas.openxmlformats.org/spreadsheetml/2006/main'
const NS_REL = 'http://schemas.openxmlformats.org/officeDocument/2006/relationships'
const NS_PKG_REL = 'http://schemas.openxmlformats.org/package/2006/relationships'

function esc(text) {
  return text.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;')
}

/** A `<t>` that keeps its spaces, as a spreadsheet writes one. */
function textTag(text) {
  const preserve = /^\s|\s$/.test(text) ? ' xml:space="preserve"' : ''
  return `<t${preserve}>${esc(text)}</t>`
}

// The cells a sheet is built from. Each returns the parts the writer needs,
// named for what an operator would call the value, not for the XML attribute
// it happens to land in.
const S = (text) => ({ kind: 'shared', text }) // text, in the shared string table
const IN = (text) => ({ kind: 'inline', text }) // text, written in the cell itself
const N = (text) => ({ kind: 'number', text: String(text) })
const D = (value) => ({ kind: 'number', text: String(value), style: 1 }) // a date, numFmt 14
const DT = (value) => ({ kind: 'number', text: String(value), style: 2 }) // date and time, numFmt 22
const SCI = (text) => ({ kind: 'number', text: String(text), style: 3 }) // shown as 0.00E+00, numFmt 11
const B = (value) => ({ kind: 'boolean', value })
const F = (formula, cached, asText = false) => ({ kind: 'formula', formula, cached, asText })
/** A cell that exists and holds nothing. */
const BLANK = { kind: 'blank' }

const STYLE_COUNT = 4

/** `cellXfs`: the default, then the three formats the fixtures use. */
const styles = `${XML_HEAD}<styleSheet xmlns="${NS_MAIN}">
<fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts>
<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>
<borders count="1"><border/></borders>
<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>
<cellXfs count="${STYLE_COUNT}">
<xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>
<xf numFmtId="14" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>
<xf numFmtId="22" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>
<xf numFmtId="11" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>
</cellXfs>
<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles>
</styleSheet>`

function columnName(index) {
  let name = ''
  for (let n = index + 1; n > 0; n = Math.floor((n - 1) / 26)) {
    name = String.fromCharCode(65 + ((n - 1) % 26)) + name
  }
  return name
}

/**
 * One workbook as bytes. `sheets` are in the order a spreadsheet shows them,
 * and `strings` is the shared string table the caller filled while building.
 *
 * A sheet may set `firstRow` when its used range does not start at A1 — a
 * spreadsheet writes no `<row>` for a blank row before the first one it has
 * something to say about, so those rows are simply absent from the file.
 */
function workbook(sheets, strings) {
  const sheetParts = sheets.map((sheet, index) => {
    const offset = (sheet.firstRow ?? 1) - 1
    const rows = sheet.rows
      .map((cells, rowIndex) => {
        const row = rowIndex + 1 + offset
        const inner = cells
          .map((cell, column) => {
            if (!cell) return ''
            const ref = `${columnName(column)}${row}`
            switch (cell.kind) {
              case 'blank':
                return `<c r="${ref}"/>`
              case 'shared':
                return `<c r="${ref}" t="s"><v>${strings.indexOf(cell.text)}</v></c>`
              case 'inline':
                return `<c r="${ref}" t="inlineStr"><is>${textTag(cell.text)}</is></c>`
              case 'number':
                return `<c r="${ref}"${cell.style ? ` s="${cell.style}"` : ''}><v>${esc(cell.text)}</v></c>`
              case 'boolean':
                return `<c r="${ref}" t="b"><v>${cell.value ? 1 : 0}</v></c>`
              case 'formula':
                return `<c r="${ref}"${cell.asText ? ' t="str"' : ''}><f>${esc(cell.formula)}</f><v>${esc(String(cell.cached))}</v></c>`
              default:
                throw new Error(`unknown cell ${JSON.stringify(cell)}`)
            }
          })
          .join('')
        return `<row r="${row}">${inner}</row>`
      })
      .join('')
    const merges = sheet.merges?.length
      ? `<mergeCells count="${sheet.merges.length}">${sheet.merges.map((ref) => `<mergeCell ref="${ref}"/>`).join('')}</mergeCells>`
      : ''
    return {
      name: `xl/worksheets/sheet${index + 1}.xml`,
      data: `${XML_HEAD}<worksheet xmlns="${NS_MAIN}" xmlns:r="${NS_REL}"><sheetData>${rows}</sheetData>${merges}</worksheet>`,
    }
  })

  const contentTypes = `${XML_HEAD}<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
<Default Extension="xml" ContentType="application/xml"/>
<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
${sheets.map((_, index) => `<Override PartName="/xl/worksheets/sheet${index + 1}.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`).join('\n')}
<Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>
<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
</Types>`

  const rootRels = `${XML_HEAD}<Relationships xmlns="${NS_PKG_REL}"><Relationship Id="rId1" Type="${NS_REL}/officeDocument" Target="xl/workbook.xml"/></Relationships>`

  const workbookXml = `${XML_HEAD}<workbook xmlns="${NS_MAIN}" xmlns:r="${NS_REL}"><sheets>${sheets
    .map((sheet, index) => `<sheet name="${esc(sheet.name)}" sheetId="${index + 1}" r:id="rId${index + 1}"/>`)
    .join('')}</sheets></workbook>`

  const workbookRels = `${XML_HEAD}<Relationships xmlns="${NS_PKG_REL}">
${sheets.map((_, index) => `<Relationship Id="rId${index + 1}" Type="${NS_REL}/worksheet" Target="worksheets/sheet${index + 1}.xml"/>`).join('\n')}
<Relationship Id="rId${sheets.length + 1}" Type="${NS_REL}/sharedStrings" Target="sharedStrings.xml"/>
<Relationship Id="rId${sheets.length + 2}" Type="${NS_REL}/styles" Target="styles.xml"/>
</Relationships>`

  const sharedStrings = `${XML_HEAD}<sst xmlns="${NS_MAIN}" count="${strings.length}" uniqueCount="${strings.length}">${strings
    .map((text) => `<si>${textTag(text)}</si>`)
    .join('')}</sst>`

  return zip([
    { name: '[Content_Types].xml', data: contentTypes },
    { name: '_rels/.rels', data: rootRels },
    { name: 'xl/workbook.xml', data: workbookXml },
    { name: 'xl/_rels/workbook.xml.rels', data: workbookRels },
    { name: 'xl/styles.xml', data: styles },
    { name: 'xl/sharedStrings.xml', data: sharedStrings },
    ...sheetParts,
  ])
}

/** Collects the shared strings of one workbook, in the order they appear. */
function stringTable(sheets) {
  const strings = []
  for (const sheet of sheets) {
    for (const row of sheet.rows) {
      for (const cell of row) {
        if (cell?.kind === 'shared' && !strings.includes(cell.text)) strings.push(cell.text)
      }
    }
  }
  return strings
}

/**
 * The serial a spreadsheet stores for a date. The epoch is Excel's 1899-12-30
 * (its 1900 leap-year bug is already folded into that date), and days are
 * counted from it.
 *
 * The times used below are whole quarter-days — 06:00, 12:00, 18:45 — because
 * those are the fractions of a day a float holds exactly. A time like 23:59:59
 * is not, and the reader multiplies the serial back out again, which is enough
 * to land a second early. The fixture would then pin that, instead of pinning
 * what the spreadsheet shows.
 */
function serial(year, month, day, hour = 0, minute = 0, second = 0) {
  const epoch = Date.UTC(1899, 11, 30)
  const days = Math.round((Date.UTC(year, month - 1, day) - epoch) / 86_400_000)
  const seconds = hour * 3600 + minute * 60 + second
  return days + seconds / 86_400
}

// ---------------------------------------------------------------------------
// contacts — the workbook and the CSV that must read the same.
//
// The numbers are numeric cells here and plain text in the CSV. Both must end
// up as the same 11 digits: this is the case a naive `Number()` turns into
// `13800138000` only by luck and into `1.3800138e10` for a longer number.

// One grid, so the two files cannot drift: the columns that hold a number are
// numeric cells in the workbook and bare text in the CSV, and both have to read
// back as the same 11 digits.
const CONTACTS = [
  ['手机号', '姓名', '时区', '备注'],
  [13800138000, '王伟', 'Asia/Shanghai', '想了解套餐'],
  [13900139000, '李静', 'Asia/Shanghai', ''],
  [13300133000, '陈涛', 'Asia/Shanghai', '续费'],
]

/** A contact cell: a number is a numeric cell, text is text, nothing is blank. */
function contactCell(cell) {
  if (typeof cell === 'number') return N(cell)
  return cell === '' ? BLANK : S(cell)
}

const contactsXlsx = {
  name: '联系人',
  rows: CONTACTS.map((cells, row) => cells.map((cell) => (row === 0 ? S(String(cell)) : contactCell(cell)))),
}

const contactsCsv = CONTACTS.map((cells) => cells.map(String).join(',')).join('\n') + '\n'

// ---------------------------------------------------------------------------
// edge cases — the cells a CSV cannot express.

const EDGE_HEADER = ['手机号', '姓名', '开通日期', '最后联系', '已确认', '行数']

const edgeSheet = {
  name: '杂项',
  rows: [
    EDGE_HEADER.map(S),
    // A number typed as text, so the leading zero is part of the value.
    [IN('013800138000'), S('王伟'), D(serial(2026, 10, 13)), DT(serial(2026, 10, 13, 12, 0, 0)), B(true), F('2*3', 6)],
    // Stored by the writer in scientific notation — expanded, never floated.
    [N('1.3800138E+10'), S('李静'), D(serial(2026, 1, 2)), DT(serial(2026, 1, 2, 6, 0, 0)), B(false), F('1+1', 2)],
    // Exactly representable as a number, but *displayed* as 1.38E+10. The
    // digits are what gets dialled, so the display format must not win.
    [SCI('13800138000'), S('陈涛'), D(serial(2026, 12, 31)), DT(serial(2026, 12, 31, 18, 45, 0)), B(true), F('2*3', 6)],
    // A blank line: not a contact, dropped like a blank line in a CSV.
    [],
    // A merged range: the value sits in the anchor, the rest are its shape.
    [S('合并单元格：只有 A6 有值'), BLANK, BLANK, BLANK, BLANK, BLANK],
    // Empty rows at the end, which a spreadsheet leaves behind constantly.
    [],
    [],
  ],
  merges: ['A6:F6'],
}

// ---------------------------------------------------------------------------
// two sheets — what puts the sheet picker on screen.

const secondSheet = {
  name: '备用名单',
  rows: [
    ['手机号', '姓名'].map(S),
    [N('13500135000'), S('赵敏')],
  ],
}

// ---------------------------------------------------------------------------
// offset — a sheet whose first two rows have nothing in them.
//
// This is the ordinary case for a file someone typed by hand: the header is not
// on row 1 because the row above it is empty. A spreadsheet writes no `<row>`
// for those, so the file simply begins at row 3, and the importer has to keep
// the numbers the spreadsheet shows.

const offsetSheet = { ...contactsXlsx, firstRow: 3 }

// ---------------------------------------------------------------------------

const outDir = path.join(import.meta.dirname, '..', 'src', 'test', 'fixtures')
mkdirSync(outDir, { recursive: true })

/** Says one line, and no more. `console` is what a linter watches for. */
function say(text) {
  process.stdout.write(`${text}\n`)
}

function write(name, data) {
  writeFileSync(path.join(outDir, name), data)
  const bytes = Buffer.isBuffer(data) ? data.length : Buffer.byteLength(data)
  say(`${name.padEnd(22)} ${bytes} bytes`)
}

function build(sheets) {
  const strings = stringTable(sheets)
  return workbook(sheets, strings)
}

write('contacts.xlsx', build([contactsXlsx]))
write('contacts.csv', contactsCsv)
write('edge-cases.xlsx', build([edgeSheet]))
write('offset.xlsx', build([offsetSheet]))
write('two-sheets.xlsx', build([contactsXlsx, secondSheet]))
// A `.xlsx` that is not an archive: an operator who renamed a CSV, or a
// download that was cut short. The dialog has to say so, not throw.
write('broken.xlsx', 'this is not a workbook\n')

// `openpyxl` is an independent reader. If it opens every fixture and agrees
// about the cells, the hand-written XML is the format it claims to be.
if (process.env.SKIP_OPENPYXL !== '1') {
  const { execFileSync } = await import('node:child_process')
  const check = `
import sys, openpyxl
for name in ['contacts.xlsx', 'edge-cases.xlsx', 'offset.xlsx', 'two-sheets.xlsx']:
    book = openpyxl.load_workbook(sys.argv[1] + '/' + name, data_only=True)
    print(name, '->', book.sheetnames)
    for sheet in book.worksheets:
        for row in sheet.iter_rows(values_only=True):
            print('   ', row)
`
  try {
    say(execFileSync('/usr/local/bin/python3', ['-c', check, outDir], { encoding: 'utf8' }).trimEnd())
  } catch (error) {
    say('openpyxl could not read the fixtures:')
    say(String(error.stdout || error.message))
    process.exitCode = 1
  }
}
