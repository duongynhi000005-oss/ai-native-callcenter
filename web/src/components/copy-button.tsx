import { useState, type RefObject } from 'react'
import { flushSync } from 'react-dom'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { copyText, selectContents } from '@/lib/clipboard'

/**
 * Puts a secret on the clipboard and says what happened.
 *
 * The confirmation is the whole point: a credential copies silently, so a
 * button that does not change is indistinguishable from one that did nothing,
 * and the reader's next move is to select the text by hand. So "Copied" means
 * a write that succeeded, never one that was merely attempted; a refused copy
 * says so and selects the shown value (`target`), so Ctrl/Cmd+C finishes the
 * job. It stays confirmed: re-copying the same string has nothing new to
 * report. Key it by the value to start over for a new one.
 */
export function CopyButton({
  value,
  target,
}: {
  value: string
  /** The element showing the value, selected when the copy is refused. */
  target: RefObject<HTMLElement | null>
}) {
  const { t } = useTranslation()
  const [outcome, setOutcome] = useState<'idle' | 'copied' | 'failed'>('idle')
  return (
    <Button
      type="button"
      size="sm"
      variant="outline"
      disabled={!value}
      onClick={async () => {
        if (!value) return
        if (await copyText(value)) {
          setOutcome('copied')
          return
        }
        // Rendered first, so the selection is made on what the reader sees.
        flushSync(() => setOutcome('failed'))
        selectContents(target.current)
      }}
    >
      {outcome === 'copied'
        ? t('admin.copied')
        : outcome === 'failed'
          ? t('admin.copyFailed')
          : t('admin.copy')}
    </Button>
  )
}
