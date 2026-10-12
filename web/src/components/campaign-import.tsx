import { Download, Upload } from 'lucide-react'
import { Dialog } from 'radix-ui'
import { useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Select } from '@/components/record-dialog'
import { DataTable, TBody, THead, Td, Th, Tr } from '@/components/table'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  buildRecords, emptySummary, guessRoles, mappingProblem, mergeSummaries, rejectedCsv, runImport,
  type ColumnRole, type ImportSummary,
} from '@/lib/campaign-import'
import { IMPORT_CHUNK_SIZE, campaignApi } from '@/lib/campaigns'
import { parseCsv, type CsvTable } from '@/lib/csv'
import { describeError } from '@/lib/errors'

const PREVIEW_ROWS = 5
const SHOWN_PROBLEMS = 200

/** The file's text. FileReader decodes as UTF-8, which is what the importer asks for. */
function readText(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.addEventListener('load', () => resolve(String(reader.result ?? '')))
    reader.addEventListener('error', () => reject(reader.error))
    reader.readAsText(file, 'utf-8')
  })
}

/** Hands the operator a file made in the browser. */
function download(name: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/csv;charset=utf-8' }))
  const link = document.createElement('a')
  link.href = url
  link.download = name
  link.click()
  URL.revokeObjectURL(url)
}

/**
 * Contact import. The file is read and parsed here, the columns are mapped by
 * the operator, and the rows go to the import operation in requests of at most
 * 5000. What comes back, row by row, is shown with the line each row had in
 * the file, and the refused ones can be downloaded to fix and send again.
 */
export function CampaignImportDialog({
  campaignId,
  open,
  onOpenChange,
  onImported,
}: {
  campaignId: string
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Called whenever rows were accepted, so counters can be refreshed. */
  onImported: () => void
}) {
  const { t } = useTranslation()
  const fileInput = useRef<HTMLInputElement>(null)
  const [fileName, setFileName] = useState('')
  const [table, setTable] = useState<CsvTable | null>(null)
  const [hasBadEncoding, setHasBadEncoding] = useState(false)
  const [roles, setRoles] = useState<ColumnRole[]>([])
  const [isRunning, setIsRunning] = useState(false)
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null)
  const [summary, setSummary] = useState<ImportSummary | null>(null)

  const problem = mappingProblem(roles)
  const records = useMemo(() => (table ? buildRecords(table, roles) : []), [table, roles])
  const badShape = records.filter((r) => r.localCode).length

  const reset = () => {
    setFileName('')
    setTable(null)
    setRoles([])
    setSummary(null)
    setProgress(null)
    setHasBadEncoding(false)
  }

  const pick = async (file: File | undefined) => {
    if (!file) return
    reset()
    const text = await readText(file)
    const parsed = parseCsv(text)
    setFileName(file.name)
    setHasBadEncoding(text.includes('�'))
    setTable(parsed)
    setRoles(guessRoles(parsed.header))
  }

  const run = async (from: number, previous: ImportSummary) => {
    setIsRunning(true)
    const result = await runImport(
      records.slice(from),
      (rows) => campaignApi.importContacts(campaignId, rows),
      { onProgress: (done) => setProgress({ done: from + done, total: records.length }) },
    )
    // The run counted from its own first record; the file's index is `from` further on.
    if (result.failure) result.failure.firstUnsent += from
    const merged = mergeSummaries(previous, result)
    setSummary(merged)
    setIsRunning(false)
    if (merged.imported > 0) onImported()
  }

  const problems = (summary?.entries ?? []).filter((e) => e.status !== 'IMPORTED')

  return (
    <Dialog.Root
      open={open}
      onOpenChange={(next) => {
        if (isRunning) return
        if (!next) reset()
        onOpenChange(next)
      }}
    >
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/20" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 flex max-h-[85vh] w-[820px] max-w-[95vw] -translate-x-1/2 -translate-y-1/2 flex-col rounded-md border bg-card p-4 shadow-md">
          <Dialog.Title className="text-base font-medium">{t('campaign.import.title')}</Dialog.Title>
          <Dialog.Description className="mt-1 text-xs text-muted-foreground">
            {t('campaign.import.hint', { max: IMPORT_CHUNK_SIZE })}
          </Dialog.Description>

          <div className="mt-4 min-h-0 flex-1 space-y-4 overflow-y-auto">
            <div className="flex items-center gap-3">
              <input
                ref={fileInput}
                type="file"
                accept=".csv,.tsv,.txt,text/csv"
                className="sr-only"
                aria-label={t('campaign.import.file')}
                onChange={(e) => void pick(e.target.files?.[0])}
              />
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={isRunning}
                onClick={() => fileInput.current?.click()}
              >
                <Upload />
                {t('campaign.import.choose')}
              </Button>
              <span className="text-xs text-muted-foreground">
                {table
                  ? t('campaign.import.parsed', {
                      name: fileName,
                      count: table.rows.length,
                      columns: table.header.length,
                    })
                  : t('campaign.import.noFile')}
              </span>
            </div>

            {table && table.header.length === 0 && (
              <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
                {t('campaign.import.empty')}
              </p>
            )}
            {table && hasBadEncoding && (
              <p role="alert" className="text-xs" style={{ color: 'var(--state-ringing)' }}>
                {t('campaign.import.badEncoding')}
              </p>
            )}
            {table?.hasUnterminatedQuote && (
              <p role="alert" className="text-xs" style={{ color: 'var(--state-ringing)' }}>
                {t('campaign.import.unterminatedQuote')}
              </p>
            )}

            {table && table.header.length > 0 && !summary && (
              <>
                <div>
                  <h3 className="mb-1 text-sm font-medium">{t('campaign.import.mapping')}</h3>
                  <p className="mb-2 text-xs text-muted-foreground">{t('campaign.import.mappingHint')}</p>
                  <DataTable>
                    <THead>
                      {table.header.map((name, index) => (
                        <Th key={index} className="min-w-36 align-top normal-case">
                          <div className="space-y-1 py-1.5">
                            <div className="truncate text-foreground" title={name}>
                              {name || t('campaign.import.unnamed', { n: index + 1 })}
                            </div>
                            <Select
                              ariaLabel={t('campaign.import.roleOf', { name })}
                              value={roles[index]?.kind ?? 'ignore'}
                              onChange={(kind) =>
                                setRoles(
                                  roles.map((r, i): ColumnRole => {
                                    if (i === index) {
                                      if (kind === 'slot') return { kind, name: name.trim() }
                                      return { kind } as ColumnRole
                                    }
                                    // Only one column is the number, and only one the zone.
                                    return (kind === 'number' || kind === 'timeZone') && r.kind === kind
                                      ? { kind: 'ignore' }
                                      : r
                                  }),
                                )
                              }
                              options={['number', 'timeZone', 'slot', 'ignore'].map((kind) => ({
                                value: kind,
                                label: t(`campaign.import.roles.${kind}`),
                              }))}
                            />
                            {roles[index]?.kind === 'slot' && (
                              <Input
                                aria-label={t('campaign.import.slotNameOf', { name })}
                                className="h-7 text-xs"
                                value={(roles[index] as { name: string }).name}
                                onChange={(e) =>
                                  setRoles(
                                    roles.map((r, i): ColumnRole =>
                                      i === index ? { kind: 'slot', name: e.target.value } : r,
                                    ),
                                  )
                                }
                              />
                            )}
                          </div>
                        </Th>
                      ))}
                    </THead>
                    <TBody>
                      {table.rows.slice(0, PREVIEW_ROWS).map((row) => (
                        <Tr key={row.line}>
                          {table.header.map((_, i) => (
                            <Td key={i} className="max-w-48 truncate text-xs">
                              {row.cells[i] ?? ''}
                            </Td>
                          ))}
                        </Tr>
                      ))}
                    </TBody>
                  </DataTable>
                  <p className="mt-1 text-xs text-muted-foreground">
                    {t('campaign.import.previewNote', {
                      shown: Math.min(PREVIEW_ROWS, table.rows.length),
                      count: table.rows.length,
                    })}
                  </p>
                </div>
                {problem && (
                  <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
                    {t(`campaign.import.problems.${problem}`)}
                  </p>
                )}
                {badShape > 0 && (
                  <p className="text-xs" style={{ color: 'var(--state-ringing)' }}>
                    {t('campaign.import.badShape', { count: badShape })}
                  </p>
                )}
              </>
            )}

            {isRunning && progress && (
              <p className="tabular text-sm" role="status">
                {t('campaign.import.progress', progress)}
              </p>
            )}

            {summary && (
              <div className="space-y-3" role="status">
                <div className="tabular flex gap-6 text-sm">
                  <span>{t('campaign.import.imported', { count: summary.imported })}</span>
                  <span>{t('campaign.import.duplicates', { count: summary.duplicates })}</span>
                  <span>{t('campaign.import.rejected', { count: summary.rejected })}</span>
                </div>
                {summary.failure && (
                  <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
                    {t('campaign.import.failed', {
                      row: summary.failure.firstUnsent + 1,
                      reason: describeError(summary.failure.error, t),
                    })}
                  </p>
                )}
                {problems.length > 0 && (
                  <>
                    <DataTable>
                      <THead>
                        <Th>{t('campaign.import.line')}</Th>
                        <Th>{t('campaign.import.result')}</Th>
                        <Th>{t('campaign.import.number')}</Th>
                        <Th>{t('campaign.import.why')}</Th>
                      </THead>
                      <TBody>
                        {problems.slice(0, SHOWN_PROBLEMS).map((entry) => (
                          <Tr key={entry.line}>
                            <Td className="tabular text-xs">{entry.line}</Td>
                            <Td className="text-xs">{t(`campaign.import.statuses.${entry.status}`)}</Td>
                            <Td className="tabular text-xs">{entry.number ?? entry.cells[0] ?? ''}</Td>
                            <Td className="text-xs text-muted-foreground">
                              {entry.code
                                ? t(`campaign.import.codes.${entry.code}`, {
                                    detail: entry.detail ?? '',
                                    defaultValue: entry.code,
                                  })
                                : t('campaign.import.duplicateNote')}
                            </Td>
                          </Tr>
                        ))}
                      </TBody>
                    </DataTable>
                    {problems.length > SHOWN_PROBLEMS && (
                      <p className="text-xs text-muted-foreground">
                        {t('campaign.import.moreProblems', { count: problems.length - SHOWN_PROBLEMS })}
                      </p>
                    )}
                  </>
                )}
                {summary.rejected > 0 && table && (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    onClick={() =>
                      download(
                        t('campaign.import.rejectedFile', { name: fileName.replace(/\.[^.]+$/, '') }),
                        rejectedCsv(table.header, summary.entries),
                      )
                    }
                  >
                    <Download />
                    {t('campaign.import.downloadRejected')}
                  </Button>
                )}
              </div>
            )}
          </div>

          <div className="flex justify-end gap-2 pt-4">
            <Dialog.Close asChild>
              <Button type="button" variant="ghost" size="sm" disabled={isRunning}>
                {summary && !summary.failure ? t('common.close') : t('common.cancel')}
              </Button>
            </Dialog.Close>
            {summary?.failure ? (
              <Button
                type="button"
                size="sm"
                disabled={isRunning}
                onClick={() => void run(summary.failure?.firstUnsent ?? 0, { ...summary, failure: undefined })}
              >
                {t('campaign.import.retry')}
              </Button>
            ) : (
              !summary && (
                <Button
                  type="button"
                  size="sm"
                  disabled={isRunning || !table || records.length === 0 || problem !== undefined}
                  onClick={() => void run(0, emptySummary())}
                >
                  {t('campaign.import.start', { count: records.length })}
                </Button>
              )
            )}
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  )
}
