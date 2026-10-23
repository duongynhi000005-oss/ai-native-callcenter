import { createFileRoute, redirect } from '@tanstack/react-router'

import { roleHomeFor } from '@/lib/nav'
import { loadSession } from '@/lib/session'

/**
 * Entry point: send each role to the landing page it actually works from.
 *
 * The check is the layout's own, so a session reads the same from here as
 * from any page below it. It always asks the server, since opening the root is
 * how a reader finds out whether they are still signed in; the user it proves
 * is kept, so the layout the redirect lands on does not ask a second time. A
 * `/me` that failed for any reason but a 401 is shown, as it is everywhere
 * else: a server restarting is not a sign-out.
 */
export const Route = createFileRoute('/')({
  beforeLoad: async ({ context }) => {
    const user = await loadSession(context.queryClient)
    throw redirect({ to: user ? roleHomeFor(user.role) : '/login' })
  },
})
