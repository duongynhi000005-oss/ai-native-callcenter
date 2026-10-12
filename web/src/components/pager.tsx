import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'

/** Previous / next under a table, shown only when there is more than a page. */
export function Pager({
  page,
  pageSize,
  total,
  onPage,
}: {
  page: number
  pageSize: number
  total: number
  onPage: (page: number) => void
}) {
  const { t } = useTranslation()
  if (total <= pageSize) return null
  const pages = Math.max(1, Math.ceil(total / pageSize))
  return (
    <div className="mt-3 flex items-center justify-between text-xs text-muted-foreground">
      <span className="tabular">{t('common.pageOf', { page: page + 1, pages, total })}</span>
      <span className="flex gap-2">
        <Button size="sm" variant="ghost" disabled={page === 0} onClick={() => onPage(page - 1)}>
          {t('common.previous')}
        </Button>
        <Button size="sm" variant="ghost" disabled={page + 1 >= pages} onClick={() => onPage(page + 1)}>
          {t('common.next')}
        </Button>
      </span>
    </div>
  )
}
