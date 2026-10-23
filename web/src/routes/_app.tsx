import { useEffect } from 'react'
import { Outlet, createFileRoute, redirect, useNavigate, useRouterState } from '@tanstack/react-router'

import { AppShell } from '@/components/app-shell'
import { BreadcrumbDetailProvider } from '@/lib/breadcrumb'
import { PhoneOnboarding } from '@/components/phone-onboarding'
import { SoftphoneBar } from '@/components/softphone-bar'
import { usePresence } from '@/lib/agent'
import { loadSession, useSession } from '@/lib/session'
import { PhoneBridgeProvider, usePhoneBridgeValue } from '@/lib/phone-bridge'
import { EventStreamProvider, useEventStream } from '@/lib/use-event-stream'

/**
 * How long a user proved signed in may be taken without asking `/me` again.
 * It covers a hand-off, not a stay: the entry route and the sign-in form both
 * prove the session and then navigate straight here, and asking again would
 * put a second round trip in front of the layout and the phone's first hello
 * (#292, #324). A navigation that comes later than that asks, which is how a
 * session that expired or was revoked under an open tab is caught: there is no
 * other place that notices.
 */
const SESSION_HANDOFF_MS = 3_000

/**
 * Authenticated layout. Everything below this route needs a session, so the
 * check happens once here rather than in every page.
 */
export const Route = createFileRoute('/_app')({
  beforeLoad: async ({ context }) => {
    const user = await loadSession(context.queryClient, { reuseProvedWithinMs: SESSION_HANDOFF_MS })
    if (!user) throw redirect({ to: '/login' })
    return { user }
  },
  component: AppLayout,
})

function AppLayout() {
  const { data: user } = useSession()
  const navigate = useNavigate()
  const { status, listeners } = useEventStream(Boolean(user))
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  const isAgent = user?.role === 'AGENT'
  const { data: presence } = usePresence(Boolean(isAgent))
  // Only an agent has a phone, so only an agent's page talks to the extension
  // or mints a SIP session. The context is still provided to everyone below,
  // because signing out goes through it whoever is doing it.
  const phone = usePhoneBridgeValue(isAgent, presence?.extensionNumber, presence?.sipDomain)

  // A session that ends under an open page (it expired, it was revoked) is
  // the sign-in page's business; a blank tab tells nobody anything.
  // Only a 401 resolves the session to null; a failed fetch is not a sign-out.
  const isSessionGone = user === null
  useEffect(() => {
    if (isSessionGone) void navigate({ to: '/login', replace: true })
  }, [isSessionGone, navigate])

  if (!user) return null

  // Only an agent has a phone to be reachable on, and only once signed in.
  const deviceState =
    isAgent && presence && presence.state !== 'LOGGED_OUT'
      ? presence.availability === 'DEVICE_UNREACHABLE'
        ? ('unreachable' as const)
        : ('reachable' as const)
      : undefined

  return (
    // One EventSource feeds the whole application; this makes its tail and its
    // health reachable from any panel below, without a second connection.
    <EventStreamProvider value={{ status, listeners }}>
      <PhoneBridgeProvider value={phone}>
        {/* The shell draws the trail; the page below names what it is about. */}
        <BreadcrumbDetailProvider>
          <AppShell
            user={user}
            streamStatus={status}
            pathname={pathname}
            deviceState={deviceState}
            // An agent carries their call controls with them on every page.
            softphone={isAgent ? <SoftphoneBar /> : undefined}
          >
            <Outlet />
          </AppShell>
        </BreadcrumbDetailProvider>
        {/* Setup an agent has not finished blocks the cockpit, on whichever
            page they happen to have opened. */}
        {isAgent && <PhoneOnboarding />}
      </PhoneBridgeProvider>
    </EventStreamProvider>
  )
}
