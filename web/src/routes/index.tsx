import { createFileRoute, isRedirect, redirect } from '@tanstack/react-router'

import { api } from '@/lib/api'
import { roleHomeFor } from '@/lib/nav'

/** Entry point: send each role to the landing page it actually works from. */
export const Route = createFileRoute('/')({
  beforeLoad: async () => {
    try {
      const { user } = await api.me()
      throw redirect({ to: roleHomeFor(user.role) })
    } catch (error) {
      // The role redirect is thrown inside the `try`, so this `catch` sees it
      // too. The router's own predicate is what tells the two apart: a
      // redirect is a target, and anything else is `/me` failing.
      if (isRedirect(error)) throw error
      throw redirect({ to: '/login' })
    }
  },
})
