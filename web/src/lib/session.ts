import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'

import { ApiError, api, type Identity } from './api'
import { usePhoneBridge } from './phone-bridge'

const SESSION_KEY = ['session'] as const

/**
 * The current session. An expired or absent session resolves to null rather
 * than throwing, so the router can redirect instead of showing an error.
 */
export function useSession() {
  return useQuery({
    queryKey: SESSION_KEY,
    queryFn: async (): Promise<Identity | null> => {
      try {
        const { user } = await api.me()
        return user
      } catch (error) {
        if (error instanceof ApiError && error.status === 401) return null
        throw error
      }
    },
    staleTime: 30_000,
    retry: false,
  })
}

/**
 * The session is over: forget the user, and with them every cache that was
 * theirs, so whoever signs in next in this tab starts from nothing.
 */
export function clearSession(queryClient: QueryClient) {
  queryClient.setQueryData(SESSION_KEY, null)
  void queryClient.invalidateQueries()
}

export function useLogin() {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ username, password }: { username: string; password: string }) =>
      api.login(username, password),
    onSuccess: ({ user }) => {
      queryClient.setQueryData(SESSION_KEY, user)
    },
  })
}

/**
 * Signing out, phone included.
 *
 * The extension is told to drop the credentials, and then the session ends.
 * The bridge latches on that: from here it sends no hello and mints nothing,
 * since the bridge stays enabled until the session is cleared and a hello in
 * that window would bring the widget back onto the sign-in page. (A session
 * that ends any other way is deprovisioned by the bridge itself when it stops
 * being enabled or unmounts.)
 * Revoking the SIP session is the server's part of signing out: it signs the
 * agent out of presence first and only then revokes the session and flushes
 * the registration, so the phone disappearing is no longer news — an agent
 * who is signed out cannot be sent a call.
 *
 * The browser must not do that half itself. Revoking from here flushed the
 * registration while the agent was still READY, and the switch's
 * `sofia::unregister` arrived as a phone lost by an agent still on the queue:
 * a DEVICE_LOST in the record of every clean sign-out.
 */
export function useLogout() {
  const queryClient = useQueryClient()
  const phone = usePhoneBridge()
  return useMutation({
    mutationFn: async () => {
      phone.deprovision()
      await api.logout()
    },
    onSuccess: () => {
      clearSession(queryClient)
    },
  })
}
