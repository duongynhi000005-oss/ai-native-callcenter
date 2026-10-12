import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'

import { request } from './api'

/** The do-not-call list: numbers the platform will never dial. */

export type DncNumber = components['schemas']['DncNumber']
export type DncSource = components['schemas']['DncSource']

export interface DncFilter {
  prefix?: string
  limit?: number
  offset?: number
}

export const DNC_KEY = ['dnc'] as const

export const dncApi = {
  /** `prefix` matches the start of the canonical number; the + travels encoded. */
  list: (filter: DncFilter) => {
    const search = new URLSearchParams()
    for (const [key, value] of Object.entries(filter)) {
      if (value !== undefined && value !== '') search.set(key, String(value))
    }
    const encoded = search.toString()
    return request<components['schemas']['DncNumberList']>(
      `/dnc-numbers${encoded ? `?${encoded}` : ''}`,
    )
  },
  /** Any common form; the answer carries the canonical E.164 number. */
  add: (number: string) =>
    request<DncNumber>('/dnc-numbers', {
      method: 'POST',
      body: JSON.stringify({ number, source: 'MANUAL' }),
    }),
  remove: (number: string) =>
    request<void>(`/dnc-numbers/${encodeURIComponent(number)}`, { method: 'DELETE' }),
}

export function useDncNumbers(filter: DncFilter) {
  return useQuery({ queryKey: [...DNC_KEY, filter], queryFn: () => dncApi.list(filter) })
}

export function useDncMutations() {
  const queryClient = useQueryClient()
  const refresh = () => void queryClient.invalidateQueries({ queryKey: DNC_KEY })
  return {
    add: useMutation({ mutationFn: dncApi.add, onSuccess: refresh }),
    remove: useMutation({ mutationFn: dncApi.remove, onSuccess: refresh }),
  }
}
