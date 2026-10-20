import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import type { components } from '@/generated/api'

import { request } from './api'

/**
 * Accounts, and the ACD identity and phone that belong to the ones that take
 * calls. Wire types come from the generated contract, as everywhere else.
 */

export type User = components['schemas']['User']
export type UserStatus = components['schemas']['UserStatus']
export type UserCreate = components['schemas']['UserCreate']
export type UserUpdate = components['schemas']['UserUpdate']

/** Form state: a new account being typed, or an existing one being edited. */
export type UserDraft = Partial<User & UserCreate>

export const USERS_KEY = ['users'] as const

/**
 * A call-taking account showing no phone: an agent or supervisor whose
 * identity never got one (accounts from before a phone came with every
 * account). Such an agent cannot sign in. An administrator takes no calls and
 * is never given a phone.
 */
export function needsPhone(user: User): boolean {
  return (user.role === 'AGENT' || user.role === 'SUPERVISOR') && !user.extensionNumber
}

/**
 * The account as it reads, to send back unchanged. Saving a call-taking
 * account without a phone is how it is given one: the server allocates it
 * from the extension pool on the same update, so assigning a phone is not an
 * operation of its own.
 */
export function unchangedUpdate(user: User): UserUpdate {
  return {
    username: user.username,
    displayName: user.displayName,
    role: user.role,
    status: user.status,
  }
}

export const usersApi = {
  list: () => request<{ items: User[] }>('/users'),

  create: (body: UserCreate) =>
    request<User>('/users', { method: 'POST', body: JSON.stringify(body) }),

  update: (userId: string, body: UserUpdate) =>
    request<User>(`/users/${userId}`, { method: 'PUT', body: JSON.stringify(body) }),

  /** Named `password` so the audit trail redacts it by field name (C58). */
  resetPassword: (userId: string, password: string) =>
    request<void>(`/users/${userId}/password`, {
      method: 'POST',
      body: JSON.stringify({ password }),
    }),
}

export function useUsers() {
  return useQuery({ queryKey: USERS_KEY, queryFn: usersApi.list })
}

/**
 * The writes, sharing one invalidation.
 *
 * Creating an account can also allocate a phone, and saving one that takes
 * calls and has none can too, so the extension list is stale after either —
 * it is invalidated alongside.
 */
export function useUserMutations() {
  const queryClient = useQueryClient()
  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: USERS_KEY })
    void queryClient.invalidateQueries({ queryKey: ['catalog', 'extensions'] })
    void queryClient.invalidateQueries({ queryKey: ['agents'] })
  }

  return {
    create: useMutation({
      mutationFn: (body: UserCreate) => usersApi.create(body),
      onSuccess: invalidate,
    }),
    update: useMutation({
      mutationFn: ({ userId, body }: { userId: string; body: UserUpdate }) =>
        usersApi.update(userId, body),
      onSuccess: invalidate,
    }),
    /** Gives a call-taking account without a phone one, by saving it unchanged. */
    assignPhone: useMutation({
      mutationFn: (user: User) => usersApi.update(user.userId, unchangedUpdate(user)),
      onSuccess: invalidate,
    }),
    resetPassword: useMutation({
      mutationFn: ({ userId, password }: { userId: string; password: string }) =>
        usersApi.resetPassword(userId, password),
      onSuccess: invalidate,
    }),
  }
}
