import { Plus, Trash2 } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { useTranslation } from 'react-i18next'

import { Field, Input, Select } from '@/components/record-dialog'
import { Button } from '@/components/ui/button'
import type { DID, Queue } from '@/lib/catalog'
import {
  INTERVAL_OUTCOMES, RETRY_OUTCOMES, WEEKDAYS, campaignFieldError, isFieldBound, isFieldEditable,
  type Campaign, type CampaignField, type CampaignForm, type CampaignRetryOutcome,
  type CampaignStatus, type Weekday,
} from '@/lib/campaigns'
import { describeError } from '@/lib/errors'

/** IANA names the browser knows, for the zone suggestions. */
function timeZoneNames(): string[] {
  try {
    return Intl.supportedValuesOf('timeZone')
  } catch {
    return []
  }
}

/** The wire field each form key is sent as, so an edit can clear that field's refusal. */
function wireFieldOf(key: keyof CampaignForm): string {
  switch (key) {
    case 'days':
    case 'ranges':
      return 'callingWindow'
    case 'isDefaultRetry':
    case 'maxAttempts':
    case 'maxSystemRetries':
    case 'retryOutcomes':
    case 'intervalMin':
    case 'backoffBaseSec':
    case 'backoffFactor':
    case 'backoffCapSec':
      return 'retryPolicy'
    default:
      return key
  }
}

/**
 * The campaign form, for creating a DRAFT and for editing one.
 *
 * Which fields can change is decided by the status, with the same rule the
 * API applies (`isFieldEditable`): a locked field is shown, disabled, with the
 * reason beside it. The server's refusals are rendered under the input they
 * are about; one that names no input shows above the buttons.
 */
export function CampaignFormView({
  value,
  onChange,
  onSubmit,
  campaign,
  dids,
  queues,
  error,
  isSaving,
  submitLabel,
}: {
  value: CampaignForm
  onChange: (next: CampaignForm) => void
  onSubmit: () => void
  /** Present when editing, which is what decides the locks. */
  campaign?: Campaign
  dids: DID[]
  queues: Queue[]
  error?: unknown
  isSaving?: boolean
  submitLabel: string
}) {
  const { t } = useTranslation()
  const status: CampaignStatus | undefined = campaign?.status
  const can = (field: CampaignField) => isFieldEditable(status, field)
  const isFinished = status === 'COMPLETED' || status === 'CANCELLED'
  // A finished campaign says so once, at the top; per field it would repeat.
  const lockedHint = (field: CampaignField) =>
    can(field) || isFinished ? undefined : t('campaign.form.lockedHint')
  // A refusal belongs to the value that was sent. Once its field changes it no
  // longer describes anything on screen, so it goes; a new refusal (a new
  // error object) starts the list again.
  const [cleared, setCleared] = useState<{ error: unknown; fields: string[] }>({ error, fields: [] })
  const clearedFields = cleared.error === error ? cleared.fields : []
  const err = (field: string) =>
    clearedFields.includes(field) ? undefined : campaignFieldError(error, field, t)
  const set = (patch: Partial<CampaignForm>) => {
    const touched = (Object.keys(patch) as Array<keyof CampaignForm>).map(wireFieldOf)
    setCleared({ error, fields: [...new Set([...clearedFields, ...touched])] })
    onChange({ ...value, ...patch })
  }
  const hasEditable = !status || !isFinished

  // Only DIDs a campaign can dial from are offered; the one already chosen is
  // always there so a stored campaign still reads true after the number changed.
  const didOptions = [
    { value: '', label: t('campaign.form.chooseDid') },
    ...dids
      .filter((d) => (d.isEnabled && d.allowOutbound) || d.id === value.didId)
      .map((d) => ({ value: d.id, label: d.description ? `${d.number} · ${d.description}` : d.number })),
  ]
  const queueOptions = [
    { value: '', label: t('campaign.form.noQueue') },
    ...queues.map((q) => ({ value: q.id, label: q.displayName })),
  ]
  const zones = timeZoneNames()
  const submit = (event: FormEvent) => {
    event.preventDefault()
    onSubmit()
  }

  const toggleDay = (day: Weekday) =>
    set({ days: value.days.includes(day) ? value.days.filter((d) => d !== day) : [...value.days, day] })
  const toggleOutcome = (outcome: CampaignRetryOutcome) =>
    set({
      retryOutcomes: value.retryOutcomes.includes(outcome)
        ? value.retryOutcomes.filter((o) => o !== outcome)
        : [...value.retryOutcomes, outcome],
    })
  const setRange = (index: number, patch: Partial<{ start: string; end: string }>) =>
    set({ ranges: value.ranges.map((r, i) => (i === index ? { ...r, ...patch } : r)) })

  const isRetryLocked = !can('retryPolicy')
  const showRetryFields = !(value.isDefaultRetry && !campaign)

  return (
    <form onSubmit={submit} className="max-w-3xl space-y-6">
      {isFinished && (
        <p role="status" className="text-xs text-muted-foreground">
          {t('campaign.form.finishedHint')}
        </p>
      )}
      <section className="space-y-3 rounded-md border bg-card p-4">
        <h2 className="text-sm font-medium">{t('campaign.form.general')}</h2>
        <Field label={t('campaign.form.name')} error={err('name')} hint={lockedHint('name')}>
          <Input
            autoFocus={!campaign}
            disabled={!can('name')}
            maxLength={200}
            value={value.name}
            onChange={(e) => set({ name: e.target.value })}
          />
        </Field>
        <Field
          label={t('campaign.form.did')}
          error={err('didId')}
          hint={lockedHint('didId') ?? t('campaign.form.didHint')}
        >
          <Select
            value={value.didId}
            disabled={!can('didId')}
            onChange={(didId) => set({ didId })}
            options={didOptions}
          />
        </Field>
        <div className="grid grid-cols-2 gap-3">
          <Field
            label={t('campaign.form.defaultCountryCode')}
            error={err('defaultCountryCode')}
            hint={lockedHint('defaultCountryCode') ?? t('campaign.form.defaultCountryCodeHint')}
          >
            <Input
              disabled={!can('defaultCountryCode')}
              inputMode="numeric"
              maxLength={3}
              placeholder={t('campaign.form.deploymentDefault')}
              value={value.defaultCountryCode}
              onChange={(e) => set({ defaultCountryCode: e.target.value.replace(/\D/g, '') })}
            />
          </Field>
          <Field
            label={t('campaign.form.followUpQueue')}
            error={err('followUpQueueId')}
            hint={lockedHint('followUpQueueId') ?? t('campaign.form.followUpQueueHint')}
          >
            <Select
              value={value.followUpQueueId}
              disabled={!can('followUpQueueId')}
              onChange={(followUpQueueId) => set({ followUpQueueId })}
              options={queueOptions}
            />
          </Field>
        </div>
      </section>

      <section className="space-y-3 rounded-md border bg-card p-4">
        <h2 className="text-sm font-medium">{t('campaign.form.schedule')}</h2>
        <Field
          label={t('campaign.form.timeZone')}
          error={err('timeZone')}
          hint={lockedHint('timeZone') ?? t('campaign.form.timeZoneHint')}
        >
          <Input
            disabled={!can('timeZone')}
            list="campaign-time-zones"
            value={value.timeZone}
            onChange={(e) => set({ timeZone: e.target.value })}
          />
        </Field>
        <datalist id="campaign-time-zones">
          {zones.map((zone) => (
            <option key={zone} value={zone} />
          ))}
        </datalist>

        <Field
          label={t('campaign.form.days')}
          error={err('callingWindow')}
          hint={lockedHint('callingWindow')}
        >
          <span className="flex flex-wrap gap-x-4 gap-y-1.5 pt-1">
            {WEEKDAYS.map((day) => (
              <label key={day} className="flex items-center gap-1.5 text-sm">
                <input
                  type="checkbox"
                  disabled={!can('callingWindow')}
                  checked={value.days.includes(day)}
                  onChange={() => toggleDay(day)}
                />
                {t(`campaign.weekdays.${day}`)}
              </label>
            ))}
          </span>
        </Field>

        <div className="space-y-1">
          <div className="text-xs font-medium text-muted-foreground">{t('campaign.form.ranges')}</div>
          {value.ranges.map((range, index) => (
            <div key={index} className="flex items-center gap-2">
              <Input
                aria-label={t('campaign.form.rangeStart', { n: index + 1 })}
                className="w-24 tabular"
                disabled={!can('callingWindow')}
                maxLength={5}
                placeholder="09:00"
                value={range.start}
                onChange={(e) => setRange(index, { start: e.target.value })}
              />
              <span className="text-muted-foreground">–</span>
              <Input
                aria-label={t('campaign.form.rangeEnd', { n: index + 1 })}
                className="w-24 tabular"
                disabled={!can('callingWindow')}
                maxLength={5}
                placeholder="18:00"
                value={range.end}
                onChange={(e) => setRange(index, { end: e.target.value })}
              />
              {can('callingWindow') && value.ranges.length > 1 && (
                <Button
                  type="button"
                  size="icon-sm"
                  variant="ghost"
                  title={t('campaign.form.removeRange')}
                  onClick={() => set({ ranges: value.ranges.filter((_, i) => i !== index) })}
                >
                  <Trash2 />
                </Button>
              )}
            </div>
          ))}
          {can('callingWindow') && (
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={() => set({ ranges: [...value.ranges, { start: '', end: '' }] })}
            >
              <Plus />
              {t('campaign.form.addRange')}
            </Button>
          )}
          <p className="text-xs text-muted-foreground">{t('campaign.form.rangesHint')}</p>
        </div>
      </section>

      <section className="space-y-3 rounded-md border bg-card p-4">
        <h2 className="text-sm font-medium">{t('campaign.form.capacity')}</h2>
        <div className="grid grid-cols-2 gap-3">
          <Field
            label={t('campaign.form.maxConcurrent')}
            error={err('maxConcurrent')}
            hint={lockedHint('maxConcurrent') ?? t('campaign.form.maxConcurrentHint')}
          >
            <Input
              type="number"
              min={1}
              max={100000}
              disabled={!can('maxConcurrent')}
              placeholder={t('campaign.form.deploymentDefault')}
              value={value.maxConcurrent}
              onChange={(e) => set({ maxConcurrent: e.target.value })}
            />
          </Field>
          <Field
            label={t('campaign.form.ringTimeoutSec')}
            error={err('ringTimeoutSec')}
            hint={lockedHint('ringTimeoutSec') ?? t('campaign.form.ringTimeoutHint')}
          >
            <Input
              type="number"
              min={5}
              max={59}
              disabled={!can('ringTimeoutSec')}
              placeholder={t('campaign.form.deploymentDefault')}
              value={value.ringTimeoutSec}
              onChange={(e) => set({ ringTimeoutSec: e.target.value })}
            />
          </Field>
        </div>
      </section>

      <section className="space-y-3 rounded-md border bg-card p-4">
        <h2 className="text-sm font-medium">{t('campaign.form.retry')}</h2>
        {!campaign && (
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={value.isDefaultRetry}
              onChange={(e) => set({ isDefaultRetry: e.target.checked })}
            />
            {t('campaign.form.useDefaultRetry')}
          </label>
        )}
        {isRetryLocked && lockedHint('retryPolicy') && (
          <p className="text-xs text-muted-foreground">{lockedHint('retryPolicy')}</p>
        )}
        {showRetryFields && (
          <>
            <div className="grid grid-cols-2 gap-3">
              <Field
                label={t('campaign.form.maxAttempts')}
                error={err('retryPolicy')}
                hint={t('campaign.form.maxAttemptsHint')}
              >
                <Input
                  type="number"
                  min={1}
                  max={20}
                  disabled={isRetryLocked}
                  value={value.maxAttempts}
                  onChange={(e) => set({ maxAttempts: e.target.value })}
                />
              </Field>
              <Field label={t('campaign.form.maxSystemRetries')} hint={t('campaign.form.maxSystemRetriesHint')}>
                <Input
                  type="number"
                  min={0}
                  max={50}
                  disabled={isRetryLocked}
                  value={value.maxSystemRetries}
                  onChange={(e) => set({ maxSystemRetries: e.target.value })}
                />
              </Field>
            </div>
            <Field label={t('campaign.form.retryOutcomes')} hint={t('campaign.form.retryOutcomesHint')}>
              <span className="flex flex-wrap gap-x-4 gap-y-1.5 pt-1">
                {RETRY_OUTCOMES.map((outcome) => (
                  <label key={outcome} className="flex items-center gap-1.5 text-sm">
                    <input
                      type="checkbox"
                      disabled={isRetryLocked}
                      checked={value.retryOutcomes.includes(outcome)}
                      onChange={() => toggleOutcome(outcome)}
                    />
                    {t(`campaign.outcomes.${outcome}`)}
                  </label>
                ))}
              </span>
            </Field>
            <div className="grid grid-cols-4 gap-3">
              {INTERVAL_OUTCOMES.map((outcome) => (
                <Field key={outcome} label={t('campaign.form.intervalMin', { outcome: t(`campaign.outcomes.${outcome}`) })}>
                  <Input
                    type="number"
                    min={1}
                    step="any"
                    disabled={isRetryLocked}
                    value={value.intervalMin[outcome]}
                    onChange={(e) =>
                      set({ intervalMin: { ...value.intervalMin, [outcome]: e.target.value } })
                    }
                  />
                </Field>
              ))}
            </div>
            <p className="text-xs text-muted-foreground">{t('campaign.form.intervalHint')}</p>
            <div className="grid grid-cols-3 gap-3">
              <Field label={t('campaign.form.backoffBase')}>
                <Input
                  type="number"
                  min={1}
                  disabled={isRetryLocked}
                  value={value.backoffBaseSec}
                  onChange={(e) => set({ backoffBaseSec: e.target.value })}
                />
              </Field>
              <Field label={t('campaign.form.backoffFactor')}>
                <Input
                  type="number"
                  min={1}
                  max={10}
                  disabled={isRetryLocked}
                  value={value.backoffFactor}
                  onChange={(e) => set({ backoffFactor: e.target.value })}
                />
              </Field>
              <Field label={t('campaign.form.backoffCap')}>
                <Input
                  type="number"
                  min={1}
                  disabled={isRetryLocked}
                  value={value.backoffCapSec}
                  onChange={(e) => set({ backoffCapSec: e.target.value })}
                />
              </Field>
            </div>
            <p className="text-xs text-muted-foreground">{t('campaign.form.backoffHint')}</p>
          </>
        )}
      </section>

      {Boolean(error) && !isFieldBound(error) && clearedFields.length === 0 && (
        <p role="alert" className="text-xs" style={{ color: 'var(--state-breach)' }}>
          {describeError(error, t)}
        </p>
      )}
      {hasEditable && (
        <div className="flex gap-2">
          <Button type="submit" size="lg" disabled={isSaving}>
            {submitLabel}
          </Button>
        </div>
      )}
    </form>
  )
}
