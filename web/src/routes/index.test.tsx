import {
  Outlet, RouterProvider, createMemoryHistory, createRootRouteWithContext, createRoute, createRouter,
} from '@tanstack/react-router'
import { QueryClient } from '@tanstack/react-query'
import { screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { RouterContext } from '@/routes/__root'
import { Route as EntryRoute } from '@/routes/index'
import { identityFixture, installBackend, renderWithProviders } from '@/test/harness'

/**
 * Opening the site root with a session already in hand.
 *
 * `/` has no page of its own: its whole job is to send a signed-in user to the
 * one they work from. It sent every one of them to the sign-in form instead,
 * because the redirect it threw for the role was caught by the `catch` written
 * for `/me` — which then threw `/login` over the top of it. The reader had to
 * sign in again on the tab they were already signed in on.
 *
 * Only the entry route is real here. Its targets are stubs that record being
 * mounted, so "landed on the cockpit" and "never showed the form on the way"
 * are two separate assertions: a flash of the sign-in page is a defect even
 * when the reader ends up in the right place.
 */

afterEach(() => {
  vi.unstubAllGlobals()
})

/** A stub page, recording the fact that it was what the reader saw. */
function stubPage(name: string, seen: string[]) {
  return function Page() {
    seen.push(name)
    return <h1>{name}</h1>
  }
}

function renderEntry() {
  const seen: string[] = []
  const root = createRootRouteWithContext<RouterContext>()({ component: Outlet })
  const tree = root.addChildren([
    // The shipped entry route's own `beforeLoad`, in a tree whose targets
    // exist: a redirect to an unregistered path is not what is under test.
    createRoute({
      getParentRoute: () => root,
      path: '/',
      beforeLoad: EntryRoute.options.beforeLoad,
    }),
    createRoute({ getParentRoute: () => root, path: '/login', component: stubPage('Sign in', seen) }),
    createRoute({ getParentRoute: () => root, path: '/agent', component: stubPage('Cockpit', seen) }),
    createRoute({
      getParentRoute: () => root,
      path: '/supervisor',
      component: stubPage('Wallboard', seen),
    }),
    createRoute({ getParentRoute: () => root, path: '/admin', component: stubPage('Overview', seen) }),
  ])
  const router = createRouter({
    routeTree: tree,
    context: { queryClient: new QueryClient({ defaultOptions: { queries: { retry: false } } }) },
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  const view = renderWithProviders(
    <RouterProvider router={router as unknown as Parameters<typeof RouterProvider>[0]['router']} />,
  )
  return { ...view, router, seen }
}

describe('the site root', () => {
  it.each([
    ['AGENT', '/agent', 'Cockpit'],
    ['SUPERVISOR', '/supervisor', 'Wallboard'],
    ['ADMIN', '/admin', 'Overview'],
  ] as const)('sends a signed-in %s to their own home', async (role, home, title) => {
    installBackend({ session: identityFixture({ role }) })
    const { router, seen } = renderEntry()

    expect(await screen.findByRole('heading', { name: title })).toBeVisible()
    expect(router.state.location.pathname).toBe(home)
    // The form is not shown on the way there, not even for one frame.
    expect(seen).toEqual([title])
  })

  it('sends a visitor with no session to the sign-in page', async () => {
    installBackend({ session: null })
    const { router } = renderEntry()

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeVisible()
    expect(router.state.location.pathname).toBe('/login')
  })

  // A `/me` that never answered is not a session that ended, but the sign-in
  // page is the only screen that can do anything about either one.
  it('sends a visitor whose /me never arrived to the sign-in page', async () => {
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))))
    const { router } = renderEntry()

    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeVisible()
    expect(router.state.location.pathname).toBe('/login')
  })
})
