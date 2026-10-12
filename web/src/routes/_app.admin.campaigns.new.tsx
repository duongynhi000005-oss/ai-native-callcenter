import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CampaignFormView } from '@/components/campaign-form'
import { PageHeader } from '@/components/page-header'
import { useDIDs, useQueues } from '@/lib/catalog'
import { blankForm, createBodyOf, useCampaignMutations } from '@/lib/campaigns'
import { requireRole } from '@/lib/guards'

/** A new campaign is a DRAFT: nothing is dialled until it is started. */
export const Route = createFileRoute('/_app/admin/campaigns/new')({
  beforeLoad: ({ context }) => requireRole(context.user, 'ADMIN', 'SUPERVISOR'),
  component: NewCampaignPage,
})

function NewCampaignPage() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const dids = useDIDs()
  const queues = useQueues()
  const { create } = useCampaignMutations()
  const [form, setForm] = useState(() => blankForm(Intl.DateTimeFormat().resolvedOptions().timeZone))

  return (
    <>
      <PageHeader title={t('campaign.new')} description={t('campaign.newHint')} />
      <CampaignFormView
        value={form}
        onChange={setForm}
        dids={dids.data?.items ?? []}
        queues={queues.data?.items ?? []}
        error={create.error}
        isSaving={create.isPending}
        submitLabel={t('campaign.create')}
        onSubmit={() =>
          create.mutate(createBodyOf(form), {
            onSuccess: (campaign) =>
              void navigate({ to: '/admin/campaigns/$campaignId', params: { campaignId: campaign.id } }),
          })
        }
      />
    </>
  )
}
