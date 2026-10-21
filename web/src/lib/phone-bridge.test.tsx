import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { act, render, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  FIRST_HELLO_TIMEOUT_MS, HELLO_ATTEMPTS, HELLO_RECHECK_MS, HELLO_TIMEOUT_MS, WEB_SIP_PHONE_EXTENSION_ID,
  probeExtensionInstalled, usePhoneBridgeValue,
  type ExtensionPresence, type PhoneBridge,
} from '@/lib/phone-bridge'
import { extensionStateFixture, installBackend, installFakeExtension, sipSessionFixture } from '@/test/harness'

/**
 * The page's half of the extension protocol.
 *
 * Two things are being pinned here. The envelope — same window, same origin,
 * our name, our version — because a page that accepts anything else has handed
 * a registration password to whoever asked. And that the credentials leave no
 * trace: they are posted and dropped, never held in state, in a cache or in
 * storage, so closing the tab is the whole of the cleanup.
 */

let bridge: PhoneBridge
/** Every reading the bridge rendered with, in order, for tests about a path. */
let readings: ExtensionPresence[] = []

const MY_EXTENSION = '1001'

function Host({
  enabled = true,
  myExtension,
  mySipDomain,
}: {
  enabled?: boolean
  myExtension?: string
  mySipDomain?: string
}) {
  bridge = usePhoneBridgeValue(enabled, myExtension, mySipDomain)
  readings.push(bridge.presence)
  return null
}

/**
 * The bridge as the shell mounts it. `myExtension` is passed separately
 * because presence loads asynchronously in the real thing: `null` is a page
 * that does not know it yet, and `learnExtension` is that request landing.
 */
function renderBridge(
  enabled = true,
  myExtension: string | null = MY_EXTENSION,
  mySipDomain?: string,
) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  })
  const tree = (ext: string | undefined) => (
    <QueryClientProvider client={queryClient}>
      <Host enabled={enabled} myExtension={ext} mySipDomain={mySipDomain} />
    </QueryClientProvider>
  )
  const view = render(tree(myExtension ?? undefined))
  return {
    ...view,
    queryClient,
    learnExtension: (ext: string) => view.rerender(tree(ext)),
  }
}

/** One message the extension would send, with the envelope under test. */
function fromExtension(
  data: Record<string, unknown>,
  envelope: { origin?: string; source?: Window | null; protocolVersion?: unknown; from?: unknown } = {},
) {
  window.dispatchEvent(
    new MessageEvent('message', {
      data: {
        source: 'from' in envelope ? envelope.from : 'web-sip-phone',
        protocolVersion: 'protocolVersion' in envelope ? envelope.protocolVersion : 1,
        ...data,
      },
      origin: envelope.origin ?? window.location.origin,
      source: 'source' in envelope ? envelope.source : window,
    }),
  )
}

/** The nonce the page put on its last hello. */
function lastHelloNonce(postMessage: ReturnType<typeof vi.spyOn>): string {
  const hellos = postMessage.mock.calls.filter(
    (call) => (call[0] as { type?: string }).type === 'hello',
  )
  const last = hellos[hellos.length - 1]
  return (last[0] as { nonce: string }).nonce
}

/** How many sessions the page has asked the platform to mint. */
function mints(api: { requests: Array<{ path: string; method: string }> }): number {
  return api.requests.filter((r) => r.method === 'POST' && r.path === '/agent/sip-session').length
}

let postMessage: ReturnType<typeof vi.spyOn>

beforeEach(() => {
  postMessage = vi.spyOn(window, 'postMessage')
  readings = []
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  delete document.documentElement.dataset.webSipPhone
})

/** Every attempt of one run of hellos gone unanswered, deadline included. */
function letEveryHelloGoUnanswered() {
  act(() => {
    vi.advanceTimersByTime(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS)
  })
}

describe('what the page posts', () => {
  it('says hello on mount, on this origin, under our name and version', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    const [message, targetOrigin] = postMessage.mock.calls[0]
    expect(message).toMatchObject({ source: 'aicc', protocolVersion: 1, type: 'hello' })
    expect((message as { nonce: string }).nonce).toBeTruthy()
    expect(targetOrigin).toBe(window.location.origin)
  })

  it('says nothing at all for an account with no phone', async () => {
    installBackend()
    renderBridge(false)
    await act(async () => {})
    expect(postMessage).not.toHaveBeenCalled()
  })

  it('hands the credentials over and keeps nothing back', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    const credentials = sipSessionFixture({ a1Hash: 'deadbeef' })
    act(() => bridge.provision(credentials))
    const [message, targetOrigin] = postMessage.mock.calls.at(-1)!
    expect(message).toMatchObject({
      source: 'aicc',
      protocolVersion: 1,
      type: 'provision',
      ...credentials,
    })
    expect(targetOrigin).toBe(window.location.origin)
  })

  it('takes the credentials back when asked', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    act(() => bridge.deprovision())
    expect(postMessage.mock.calls.at(-1)![0]).toMatchObject({
      source: 'aicc',
      protocolVersion: 1,
      type: 'deprovision',
    })
  })
})

describe('what the page accepts', () => {
  it('takes a hello reply carrying a nonce it sent', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    const nonce = lastHelloNonce(postMessage)
    act(() => fromExtension({ type: 'hello', nonce, extensionVersion: '1.4.0' }))
    await waitFor(() => expect(bridge.detected).toBe(true))
    expect(bridge.extensionVersion).toBe('1.4.0')
  })

  it.each([
    ['another origin', { origin: 'https://phish.example' }],
    ['another window', { source: null }],
    ['another sender', { from: 'not-the-phone' }],
    ['another protocol version', { protocolVersion: 2 }],
  ])('ignores a hello from %s', async (_case, envelope) => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    const nonce = lastHelloNonce(postMessage)
    act(() => fromExtension({ type: 'hello', nonce, extensionVersion: '9.9.9' }, envelope))
    await act(async () => {})
    expect(bridge.detected).toBe(false)
  })

  it('ignores a hello nobody asked for', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    act(() => fromExtension({ type: 'hello', nonce: 'made-up', extensionVersion: '9.9.9' }))
    await act(async () => {})
    expect(bridge.detected).toBe(false)
  })
})

/**
 * Detection follows the extension for as long as the page is open.
 *
 * An extension can arrive in an open tab (installed, a site allowed, updated
 * and re-injected) and leave it (disabled, removed, invalidated by an update).
 * Each of those is an event the page hears — the marker coming or going, a
 * hello answered or not — and none of them may need a refresh to be seen.
 */
describe('detection over time', () => {
  it('detects an extension injected into the tab after the page loaded', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    await act(async () => {})
    expect(bridge.detected).toBe(false)
    const extension = installFakeExtension()
    await act(async () => {
      extension.mark()
    })
    await waitFor(() => expect(bridge.detected).toBe(true))
    // Removing the marker is an event the page hears; let it land in act.
    await act(async () => extension.uninstall())
  })

  it('does not react to attribute changes that are not the marker', async () => {
    installBackend()
    const extension = installFakeExtension()
    extension.mark()
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))
    const hellos = extension.messagesOfType('hello').length
    await act(async () => {
      document.documentElement.setAttribute('data-theme', 'dark')
    })
    await act(async () => {})
    expect(extension.messagesOfType('hello')).toHaveLength(hellos)
    expect(bridge.detected).toBe(true)
    document.documentElement.removeAttribute('data-theme')
    // Removing the marker is an event the page hears; let it land in act.
    await act(async () => extension.uninstall())
  })

  /**
   * A miss is not an absence. A tab that has just woken runs its timers late
   * and its listeners later, and one silent hello there was enough to tell an
   * agent whose phone was registered the whole time that they had no
   * extension. The question is asked again before silence is believed.
   */
  it('asks again before it forgets an extension that missed one hello', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    act(() => fromExtension({ type: 'hello', nonce: lastHelloNonce(postMessage), extensionVersion: '1.4.0' }))
    await waitFor(() => expect(bridge.detected).toBe(true))

    vi.useFakeTimers()
    // The tab comes back, the page asks again, and nothing answers: an
    // instance invalidated by an update that left its marker behind.
    const before = postMessage.mock.calls.length
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    for (let attempt = 1; attempt < HELLO_ATTEMPTS; attempt += 1) {
      act(() => {
        vi.advanceTimersByTime(HELLO_TIMEOUT_MS)
      })
      // Still asking, so still detected: this one is not over yet.
      expect(bridge.detected).toBe(true)
    }
    expect(postMessage.mock.calls.length - before).toBe(HELLO_ATTEMPTS)
    act(() => {
      vi.advanceTimersByTime(HELLO_TIMEOUT_MS)
    })
    expect(bridge.detected).toBe(false)
  })

  // The extension keeps reporting its registration unprompted, and a report
  // is an extension talking. A page that only believed hello replies watched
  // the phone say REGISTERED while it told the agent to go and install one.
  it('takes a state as proof of presence after a hello went unanswered', async () => {
    installBackend()
    const extension = installFakeExtension({ answersHello: false })
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())

    vi.useFakeTimers()
    letEveryHelloGoUnanswered()
    expect(bridge.detected).toBe(false)

    act(() => {
      extension.report({ registration: 'REGISTERED' })
    })
    expect(bridge.detected).toBe(true)
    expect(bridge.state?.registration).toBe('REGISTERED')
    // And the deadline that would have unset it again is gone with the rest.
    act(() => {
      vi.advanceTimersByTime(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS * 2)
    })
    expect(bridge.detected).toBe(true)
    extension.uninstall()
  })

  it('stays detected when the hello is answered inside the deadline', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    vi.useFakeTimers()
    act(() => fromExtension({ type: 'hello', nonce: lastHelloNonce(postMessage), extensionVersion: '1.4.0' }))
    expect(bridge.detected).toBe(true)
    act(() => {
      vi.advanceTimersByTime(HELLO_TIMEOUT_MS * 2)
    })
    expect(bridge.detected).toBe(true)
  })
})

/**
 * When a session is minted, and — more to the point — when it is not.
 *
 * Minting replaces the agent's session and flushes the registration the
 * previous one held, and a registration lost while the agent is READY drops
 * them to NOT_READY/DEVICE_LOST. The extension keeps provisioned credentials
 * across a page reload, so a page that mints because it just loaded knocks a
 * working agent out of the queue for pressing F5. That shipped and was caught
 * on a live call.
 */
describe('provisioning', () => {
  it('mints nothing for a phone already holding our session', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'PROVISIONED', account: MY_EXTENSION, registration: 'REGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))
    await act(async () => {})
    expect(mints(api)).toBe(0)
    expect(extension.messagesOfType('provision')).toHaveLength(0)
    extension.uninstall()
  })

  it('mints for a phone that reports holding nothing', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(mints(api)).toBe(1))
    await waitFor(() => expect(extension.messagesOfType('provision')).toHaveLength(1))
    const { expiresAt, ...credentials } = sipSessionFixture()
    expect(expiresAt).toBeTruthy()
    expect(extension.messagesOfType('provision')[0]).toMatchObject(credentials)
    extension.uninstall()
  })

  it('does not mint twice for the same thing being wrong twice', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(mints(api)).toBe(1))
    await act(async () => {
      extension.report({ credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' })
    })
    await act(async () => {})
    expect(mints(api)).toBe(1)
    extension.uninstall()
  })

  // A build that predates provisionStatus sends none, and one that is newer
  // may send a value this page does not know; either way the old rules stand.
  it.each([
    ['an account somebody typed in by hand', { credentialSource: 'MANUAL' as const }],
    ['a session provisioned for somebody else', { account: '1002' }],
    ['a manual account with a status it does not recognise',
      { credentialSource: 'MANUAL' as const, provisionStatus: 'SOMETHING_ELSE' as never }],
  ])('replaces %s', async (_case, state) => {
    const api = installBackend()
    const extension = installFakeExtension({ state })
    renderBridge()
    await waitFor(() => expect(mints(api)).toBe(1))
    expect(bridge.state?.provisionStatus).toBeUndefined()
    extension.uninstall()
  })

  /**
   * The content script posts its state as soon as it attaches, which can be
   * before this page knows which extension the agent is bound to — presence
   * is a request. A state judged against an unknown extension must be judged
   * again when it is known, not dropped.
   */
  it('judges a state that arrived before presence did, once presence lands', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'PROVISIONED', account: '1002', registration: 'REGISTERED' },
    })
    const { learnExtension } = renderBridge(true, null)
    await waitFor(() => expect(bridge.state?.account).toBe('1002'))
    // Whose account 1002 is cannot be known yet, so nothing is done about it.
    expect(mints(api)).toBe(0)
    await act(async () => {
      learnExtension(MY_EXTENSION)
    })
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })

  it('mints once, not twice, for a phone with nothing when presence lands late', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    const { learnExtension } = renderBridge(true, null)
    await waitFor(() => expect(mints(api)).toBe(1))
    await act(async () => {
      learnExtension(MY_EXTENSION)
    })
    await act(async () => {})
    expect(mints(api)).toBe(1)
    extension.uninstall()
  })

  // Reloading the page re-runs everything above with an extension that kept
  // its credentials. Nothing may be minted, or the agent loses their phone
  // for pressing F5.
  it('mints nothing when the tab comes back and the phone says hello again', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'PROVISIONED', account: MY_EXTENSION, registration: 'REGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))
    const hellos = extension.messagesOfType('hello').length
    await act(async () => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await waitFor(() => expect(extension.messagesOfType('hello').length).toBeGreaterThan(hellos))
    await act(async () => {})
    expect(mints(api)).toBe(0)
    extension.uninstall()
  })

  // The registration the switch holds is not ours any more: a newer session
  // in another browser took it. Taking it back is the agent's decision.
  it('leaves a phone displaced by another browser alone until asked', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'PROVISIONED', account: MY_EXTENSION, registration: 'REGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))
    await act(async () => {
      extension.report({ registration: 'FAILED', credentialSource: 'PROVISIONED' })
    })
    await act(async () => {})
    expect(mints(api)).toBe(0)
    await act(async () => {
      bridge.reprovision()
    })
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })

  /**
   * A manual account saved in the extension's Options while a provisioned one
   * is held. The manual account is what the phone uses and ours is kept
   * dormant, so a provision posted from here would be accepted and not
   * applied — while the mint behind it still flushed, server-side, the
   * registration the manual account is holding.
   */
  it('mints nothing for a phone whose provision has been overridden', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: {
        credentialSource: 'MANUAL',
        account: MY_EXTENSION,
        registration: 'REGISTERED',
        provisionStatus: 'OVERRIDDEN',
      },
    })
    renderBridge()
    await waitFor(() => expect(bridge.state?.provisionStatus).toBe('OVERRIDDEN'))
    await act(async () => {})
    expect(mints(api)).toBe(0)
    expect(extension.messagesOfType('provision')).toHaveLength(0)
    extension.uninstall()
  })

  it('mints once the override is cleared and the phone is left with nothing', async () => {
    const api = installBackend()
    const extension = installFakeExtension({
      state: {
        credentialSource: 'MANUAL',
        account: MY_EXTENSION,
        registration: 'REGISTERED',
        provisionStatus: 'OVERRIDDEN',
      },
    })
    renderBridge()
    await waitFor(() => expect(bridge.state?.provisionStatus).toBe('OVERRIDDEN'))
    expect(mints(api)).toBe(0)
    // Signing out in the extension's Options leaves it holding nothing.
    await act(async () => {
      extension.report({
        credentialSource: 'NONE',
        account: null,
        registration: 'UNREGISTERED',
        provisionStatus: 'NONE',
      })
    })
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })

  it('stops asking once the server says no extension is bound', async () => {
    const api = installBackend({ sipSession: null })
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.provisionErrorCode).toBe('CONFLICT'))
    const attempts = mints(api)
    // A different condition, which would otherwise earn a fresh attempt.
    await act(async () => {
      extension.report({ credentialSource: 'MANUAL', account: MY_EXTENSION })
    })
    await act(async () => {})
    expect(mints(api)).toBe(attempts)
    expect(extension.messagesOfType('provision')).toHaveLength(0)
    extension.uninstall()
  })

  it('asks again when the agent presses re-provision', async () => {
    const api = installBackend({ sipSession: null })
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    renderBridge()
    await waitFor(() => expect(bridge.provisionErrorCode).toBe('CONFLICT'))
    const attempts = mints(api)
    await act(async () => {
      bridge.reprovision()
    })
    await waitFor(() => expect(mints(api)).toBe(attempts + 1))
    extension.uninstall()
  })
})

describe('a credential issued by another deployment', () => {
  const MINE = 'b.example'
  const THEIRS = 'a.example'
  const held = (sipDomain: string) => ({
    credentialSource: 'PROVISIONED' as const,
    account: MY_EXTENSION,
    sipDomain,
    registration: 'REGISTERED' as const,
  })

  it('mints once for our account held at another SIP domain', async () => {
    const api = installBackend()
    const extension = installFakeExtension({ state: held(THEIRS) })
    renderBridge(true, MY_EXTENSION, MINE)
    await waitFor(() => expect(mints(api)).toBe(1))
    await act(async () => {})
    expect(mints(api)).toBe(1)
    extension.uninstall()
  })

  it('mints nothing for our account at our own SIP domain', async () => {
    const api = installBackend()
    const extension = installFakeExtension({ state: held(MINE) })
    renderBridge(true, MY_EXTENSION, MINE)
    await waitFor(() => expect(bridge.detected).toBe(true))
    await act(async () => {})
    expect(mints(api)).toBe(0)
    extension.uninstall()
  })

  it('falls back to the account alone when either domain is unknown', async () => {
    const api = installBackend()
    const noDomain = installFakeExtension({ state: { ...held(THEIRS), sipDomain: null } })
    renderBridge(true, MY_EXTENSION, MINE)
    await waitFor(() => expect(bridge.detected).toBe(true))
    await act(async () => {})
    expect(mints(api)).toBe(0)
    noDomain.uninstall()
  })

  it('falls back to the account alone when presence has no domain', async () => {
    const api = installBackend()
    const extension = installFakeExtension({ state: held(THEIRS) })
    renderBridge(true, MY_EXTENSION, undefined)
    await waitFor(() => expect(bridge.detected).toBe(true))
    await act(async () => {})
    expect(mints(api)).toBe(0)
    extension.uninstall()
  })

  it('leaves a foreign domain alone once our own credential was seen held', async () => {
    const api = installBackend()
    const extension = installFakeExtension({ state: held(MINE) })
    renderBridge(true, MY_EXTENSION, MINE)
    await waitFor(() => expect(bridge.detected).toBe(true))
    await act(async () => {
      extension.report(held(THEIRS))
    })
    await act(async () => {})
    expect(mints(api)).toBe(0)
    await act(async () => {
      bridge.reprovision()
    })
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })

  it('forgets having held ours when the agent signs out', async () => {
    const api = installBackend()
    const extension = installFakeExtension({ state: held(MINE) })
    const view = renderBridge(true, MY_EXTENSION, MINE)
    await waitFor(() => expect(bridge.detected).toBe(true))
    view.rerender(
      <QueryClientProvider client={view.queryClient}>
        <Host enabled={false} myExtension={MY_EXTENSION} mySipDomain={MINE} />
      </QueryClientProvider>,
    )
    // Another deployment takes the phone while nobody is signed in here.
    await act(async () => {
      extension.report(held(THEIRS))
    })
    view.rerender(
      <QueryClientProvider client={view.queryClient}>
        <Host enabled myExtension={MY_EXTENSION} mySipDomain={MINE} />
      </QueryClientProvider>,
    )
    await waitFor(() => expect(mints(api)).toBe(1))
    extension.uninstall()
  })
})

/**
 * Out of reach is not uninstalled, and the marker is what tells them apart.
 *
 * The extension holds its registration in a worker that survives the machine
 * sleeping; the content script in a sleeping tab may not answer. "Lost" is
 * only honest when a content script has marked this page, because only then
 * is there something a reload would reach. No marker is an install wizard
 * from the first paint, whatever this browser did on an earlier visit.
 */
describe('an extension this page has lost', () => {
  it('is lost when the marker stays and the hellos go unanswered', async () => {
    installBackend()
    const extension = installFakeExtension()
    extension.mark()
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))
    expect(bridge.isLost).toBe(false)

    vi.useFakeTimers()
    extension.silence()
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'))
    })
    // Still asking: not lost until the last attempt has gone unanswered.
    expect(bridge.isLost).toBe(false)
    letEveryHelloGoUnanswered()
    expect(bridge.detected).toBe(false)
    expect(bridge.isLost).toBe(true)
    vi.useRealTimers()
    await act(async () => extension.uninstall())
  })

  it('stops being lost when the extension answers again', async () => {
    installBackend()
    document.documentElement.dataset.webSipPhone = '1'
    vi.useFakeTimers()
    renderBridge()
    letEveryHelloGoUnanswered()
    expect(bridge.isLost).toBe(true)
    act(() => fromExtension({ type: 'hello', nonce: lastHelloNonce(postMessage), extensionVersion: '1.4.0' }))
    expect(bridge.detected).toBe(true)
    expect(bridge.isLost).toBe(false)
  })

  // The marker going is not a verdict (issue #187): it starts a fresh run of
  // hellos, and the page keeps believing what it knew until that run ends.
  // Silence with no marker on the page is then absent, not lost.
  it('is absent, not lost, once the marker has gone and the fresh run is unanswered', async () => {
    installBackend()
    const extension = installFakeExtension()
    extension.mark()
    renderBridge()
    await waitFor(() => expect(bridge.detected).toBe(true))

    vi.useFakeTimers()
    await act(async () => {
      extension.uninstall()
    })
    expect(bridge.presence).toBe('PRESENT')
    letEveryHelloGoUnanswered()
    expect(bridge.presence).toBe('ABSENT')
    expect(bridge.detected).toBe(false)
    expect(bridge.isLost).toBe(false)
  })

  it('is absent, not lost, on a page nothing has marked', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    await act(async () => {})
    expect(bridge.detected).toBe(false)
    expect(bridge.isLost).toBe(false)
  })

  // The marker alone is not an accusation: the content script that wrote it
  // may simply not have heard the first hello yet.
  it('is not lost while the first run of hellos is still in flight', async () => {
    installBackend()
    document.documentElement.dataset.webSipPhone = '1'
    vi.useFakeTimers()
    renderBridge()
    expect(postMessage).toHaveBeenCalled()
    expect(bridge.isLost).toBe(false)
    act(() => {
      vi.advanceTimersByTime(HELLO_TIMEOUT_MS)
    })
    expect(bridge.isLost).toBe(false)
  })

  // The page rendered before the content script injected: nothing says lost
  // until the marker is there and has gone unanswered.
  it('treats a marker that lands late as an extension to ask, not one lost', async () => {
    installBackend()
    vi.useFakeTimers()
    renderBridge()
    expect(bridge.isLost).toBe(false)
    letEveryHelloGoUnanswered()
    expect(bridge.isLost).toBe(false)

    vi.useRealTimers()
    const extension = installFakeExtension()
    await act(async () => {
      extension.mark()
    })
    await waitFor(() => expect(bridge.detected).toBe(true))
    expect(bridge.isLost).toBe(false)
    await act(async () => extension.uninstall())
  })

  it('does not read any memory of an earlier visit: no marker is the wizard at once', async () => {
    window.localStorage.setItem('aicc.phone.extensionSeen', '1')
    installBackend()
    renderBridge()
    expect(bridge.isLost).toBe(false)
    await act(async () => {})
    expect(bridge.isLost).toBe(false)
  })
})

describe('where the credentials do not go', () => {
  it('writes no part of them to storage or to the query cache', async () => {
    const setItem = vi.spyOn(Storage.prototype, 'setItem')
    installBackend()
    const extension = installFakeExtension({
      state: { credentialSource: 'NONE', account: null, registration: 'UNREGISTERED' },
    })
    const { queryClient } = renderBridge()
    await waitFor(() => expect(extension.messagesOfType('provision')).not.toHaveLength(0))

    // The extension was given the hash, so this is not a test of nothing.
    expect(extension.messagesOfType('provision')[0].a1Hash).toBe(sipSessionFixture().a1Hash)

    const written = setItem.mock.calls.map(([, value]) => String(value)).join('\n')
    expect(written).not.toContain('a1Hash')
    expect(written).not.toContain(sipSessionFixture().a1Hash)

    const cached = JSON.stringify(
      queryClient.getQueryCache().getAll().map((query) => ({ key: query.queryKey, data: query.state.data })),
    )
    expect(cached).not.toContain('a1Hash')
    expect(cached).not.toContain(sipSessionFixture().a1Hash)
    extension.uninstall()
  })
})

describe('the install probe', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.unstubAllEnvs()
  })

  it('fetches the exposed options page of the configured extension', async () => {
    const fetchMock = vi.fn(async () => new Response(null))
    vi.stubGlobal('fetch', fetchMock)
    expect(await probeExtensionInstalled()).toBe(true)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const [url] = fetchMock.mock.calls[0] as unknown as [string]
    expect(url).toBe(`chrome-extension://${WEB_SIP_PHONE_EXTENSION_ID}/options.html`)
  })

  it('probes the id this build was given when it was given one', async () => {
    const id = 'a'.repeat(32)
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', id)
    const fetchMock = vi.fn(async () => new Response(null))
    vi.stubGlobal('fetch', fetchMock)
    await probeExtensionInstalled()
    expect((fetchMock.mock.calls[0] as unknown as [string])[0]).toBe(
      `chrome-extension://${id}/options.html`,
    )
  })

  it('reads a rejected request as not installed', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    expect(await probeExtensionInstalled()).toBe(false)
  })

  it('is false without a fetch to ask', async () => {
    vi.stubGlobal('fetch', undefined)
    expect(await probeExtensionInstalled()).toBe(false)
  })
})

/**
 * The four readings, and how a page gets from one to another.
 *
 * Until something answers, or a whole run goes unanswered, the page does not
 * know; and a marker removed is a reason to ask again, not to conclude
 * (issue #187: a wizard that told an agent to allow a site already allowed).
 */
describe('what the page knows about the extension', () => {
  const marker = {
    set: () => {
      document.documentElement.dataset.webSipPhone = '1'
    },
    clear: () => {
      delete document.documentElement.dataset.webSipPhone
    },
  }

  it('does not know until something answers or a whole run is silent', async () => {
    installBackend()
    // A marked page: an unmarked one is given a short first run instead.
    marker.set()
    vi.useFakeTimers()
    renderBridge()
    expect(bridge.presence).toBe('UNKNOWN')
    act(() => {
      vi.advanceTimersByTime(HELLO_TIMEOUT_MS * (HELLO_ATTEMPTS - 1))
    })
    expect(bridge.presence).toBe('UNKNOWN')
  })

  it('is present on a hello reply', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    act(() => fromExtension({ type: 'hello', nonce: lastHelloNonce(postMessage), extensionVersion: '1.4.0' }))
    expect(bridge.presence).toBe('PRESENT')
    expect(bridge.detected).toBe(true)
  })

  it('is present on a state alone', async () => {
    installBackend()
    renderBridge()
    await waitFor(() => expect(postMessage).toHaveBeenCalled())
    act(() => fromExtension({ type: 'state', ...extensionStateFixture() }))
    expect(bridge.presence).toBe('PRESENT')
  })

  it('is absent after a silent run with no marker, and lost with one', async () => {
    installBackend()
    vi.useFakeTimers()
    renderBridge()
    letEveryHelloGoUnanswered()
    expect(bridge.presence).toBe('ABSENT')
    expect(bridge.isLost).toBe(false)
  })

  it('is lost after a silent run with the marker on the page', async () => {
    installBackend()
    marker.set()
    vi.useFakeTimers()
    renderBridge()
    letEveryHelloGoUnanswered()
    expect(bridge.presence).toBe('LOST')
    expect(bridge.isLost).toBe(true)
  })

  it('moves between lost and absent as the marker comes and goes after the run', async () => {
    installBackend()
    marker.set()
    vi.useFakeTimers()
    renderBridge()
    letEveryHelloGoUnanswered()
    expect(bridge.presence).toBe('LOST')
    await act(async () => {
      marker.clear()
    })
    expect(bridge.presence).toBe('ABSENT')
  })

  // An update replaces the extension in one task: marker out, marker in.
  it('stays present when the marker is removed and re-added in the same task', async () => {
    installBackend()
    const extension = installFakeExtension()
    marker.set()
    renderBridge()
    await waitFor(() => expect(bridge.presence).toBe('PRESENT'))
    await act(async () => {
      marker.clear()
      marker.set()
    })
    expect(bridge.presence).toBe('PRESENT')
    await act(async () => {})
    expect(bridge.presence).toBe('PRESENT')
    await act(async () => extension.uninstall())
  })

  it('stays present, and never reads absent, when the marker returns a task later', async () => {
    installBackend()
    const extension = installFakeExtension()
    marker.set()
    renderBridge()
    await waitFor(() => expect(bridge.presence).toBe('PRESENT'))
    const seen: string[] = []
    await act(async () => {
      marker.clear()
    })
    seen.push(bridge.presence)
    await act(async () => {
      marker.set()
    })
    seen.push(bridge.presence)
    await waitFor(() => expect(extension.messagesOfType('hello').length).toBeGreaterThan(2))
    seen.push(bridge.presence)
    expect(seen).toEqual(['PRESENT', 'PRESENT', 'PRESENT'])
    await act(async () => extension.uninstall())
  })

  it('asks again when the marker goes, without dropping what it knew', async () => {
    installBackend()
    const extension = installFakeExtension()
    marker.set()
    renderBridge()
    await waitFor(() => expect(bridge.presence).toBe('PRESENT'))
    const before = extension.messagesOfType('hello').length
    await act(async () => {
      marker.clear()
    })
    await waitFor(() => expect(extension.messagesOfType('hello').length).toBeGreaterThan(before))
    expect(bridge.presence).toBe('PRESENT')
    await act(async () => extension.uninstall())
  })

  it('forgets everything when the account stops being an agent, and starts from unknown', async () => {
    installBackend()
    const extension = installFakeExtension()
    const view = renderBridge(true)
    await waitFor(() => expect(bridge.presence).toBe('PRESENT'))
    const tree = (enabled: boolean) => (
      <QueryClientProvider client={view.queryClient}>
        <Host enabled={enabled} myExtension={MY_EXTENSION} />
      </QueryClientProvider>
    )
    view.rerender(tree(false))
    expect(bridge.presence).toBe('UNKNOWN')
    extension.silence()
    view.rerender(tree(true))
    expect(bridge.presence).toBe('UNKNOWN')
    extension.uninstall()
    const second = installFakeExtension()
    act(() => bridge.sendHello())
    await waitFor(() => expect(bridge.presence).toBe('PRESENT'))
    second.uninstall()
  })

  it('asks again when the window regains focus', async () => {
    installBackend()
    const extension = installFakeExtension()
    renderBridge()
    await waitFor(() => expect(bridge.presence).toBe('PRESENT'))
    const before = extension.messagesOfType('hello').length
    act(() => {
      window.dispatchEvent(new Event('focus'))
    })
    await waitFor(() => expect(extension.messagesOfType('hello').length).toBe(before + 1))
    await act(async () => extension.uninstall())
  })

  describe('the slow recheck', () => {
    it('asks again every so often while nothing answers, and recovers when it does', async () => {
      installBackend()
      vi.useFakeTimers()
      renderBridge()
      letEveryHelloGoUnanswered()
      expect(bridge.presence).toBe('ABSENT')
      const before = postMessage.mock.calls.length
      act(() => {
        vi.advanceTimersByTime(HELLO_RECHECK_MS)
      })
      expect(postMessage.mock.calls.length).toBeGreaterThan(before)
      // The run is a question like any other: still absent while it is open.
      expect(bridge.presence).toBe('ABSENT')
      act(() => fromExtension({ type: 'hello', nonce: lastHelloNonce(postMessage), extensionVersion: '1.4.0' }))
      expect(bridge.presence).toBe('PRESENT')
    })

    it('stops asking once the extension answers', async () => {
      installBackend()
      vi.useFakeTimers()
      renderBridge()
      letEveryHelloGoUnanswered()
      act(() => {
        vi.advanceTimersByTime(HELLO_RECHECK_MS)
      })
      act(() => fromExtension({ type: 'hello', nonce: lastHelloNonce(postMessage), extensionVersion: '1.4.0' }))
      const after = postMessage.mock.calls.length
      act(() => {
        vi.advanceTimersByTime(HELLO_RECHECK_MS * 5)
      })
      expect(postMessage.mock.calls.length).toBe(after)
    })

    it('does not ask while unknown or when the page is not an agent', async () => {
      installBackend()
      vi.useFakeTimers()
      renderBridge(false)
      act(() => {
        vi.advanceTimersByTime(HELLO_RECHECK_MS * 3)
      })
      expect(postMessage).not.toHaveBeenCalled()
    })
  })
})

/** A document still loading, until the test says it has loaded. */
function loadingDocument() {
  let readyState: DocumentReadyState = 'loading'
  Object.defineProperty(document, 'readyState', { configurable: true, get: () => readyState })
  return {
    finish: () =>
      act(() => {
        readyState = 'complete'
        window.dispatchEvent(new Event('load'))
      }),
  }
}

/** A tab the agent cannot see yet, until the test shows it. */
function hiddenTab() {
  let visibilityState: DocumentVisibilityState = 'hidden'
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibilityState })
  return {
    show: () =>
      act(() => {
        visibilityState = 'visible'
        document.dispatchEvent(new Event('visibilitychange'))
      }),
  }
}

function elapse(ms: number) {
  act(() => {
    vi.advanceTimersByTime(ms)
  })
}

/** How long, from now, until the bridge reads `reading`; -1 if not by `cap`. */
function msUntil(reading: ExtensionPresence, { step = 1, cap = 20_000 } = {}): number {
  for (let elapsed = 0; elapsed <= cap; elapsed += step) {
    if (bridge.presence === reading) return elapsed
    elapse(step)
  }
  return -1
}

function answerLastHello() {
  act(() => fromExtension({ type: 'hello', nonce: lastHelloNonce(postMessage), extensionVersion: '1.4.0' }))
}

function hellosPosted(): number {
  return postMessage.mock.calls.filter((call) => (call[0] as { type?: string }).type === 'hello').length
}

/** The order of distinct readings, repeats folded. */
function path(from: ExtensionPresence[]): ExtensionPresence[] {
  return from.filter((reading, i) => i === 0 || from[i - 1] !== reading)
}

/**
 * The first verdict on a page nothing has marked (issue #292).
 *
 * A content script writes its marker as it is injected, and Chrome injects it
 * no later than just after `load`. A page still unmarked one task after that
 * has no content script, so it is told so after one short hello rather than a
 * whole run: an agent without the extension used to look at a cockpit that
 * seemed to work for six seconds before the setup card came up. A marked page
 * keeps the whole run, which is what a busy or just-woken tab needs.
 */
describe('the first verdict on a page nothing has marked', () => {
  afterEach(() => {
    Reflect.deleteProperty(document, 'readyState')
    Reflect.deleteProperty(document, 'visibilityState')
  })

  it('reads absent a short deadline after load, not after a whole run', () => {
    installBackend()
    const page = loadingDocument()
    vi.useFakeTimers()
    renderBridge()
    // Loading is not silence: nothing is concluded before `load`.
    elapse(1000)
    expect(bridge.presence).toBe('UNKNOWN')
    const before = hellosPosted()
    page.finish()
    // One task later, the page asks once more and waits the short deadline.
    elapse(0)
    expect(hellosPosted()).toBe(before + 1)
    expect(msUntil('ABSENT')).toBe(FIRST_HELLO_TIMEOUT_MS)
    expect(FIRST_HELLO_TIMEOUT_MS).toBeLessThan(HELLO_TIMEOUT_MS)
    expect(bridge.isLost).toBe(false)
  })

  it('does not move its deadline for focus and tab switches in the meantime', () => {
    installBackend()
    vi.useFakeTimers()
    renderBridge()
    elapse(0)
    const before = hellosPosted()
    for (let i = 0; i < 5; i += 1) {
      elapse(90)
      act(() => {
        window.dispatchEvent(new Event('focus'))
        document.dispatchEvent(new Event('visibilitychange'))
      })
    }
    // Each was asked, as always; none of them reset the clock.
    expect(hellosPosted()).toBe(before + 10)
    expect(bridge.presence).toBe('UNKNOWN')
    expect(msUntil('ABSENT')).toBe(FIRST_HELLO_TIMEOUT_MS - 5 * 90)
  })

  it('reaches the same verdict under StrictMode, whose effects run twice', () => {
    installBackend()
    vi.useFakeTimers()
    const queryClient = new QueryClient()
    render(
      <StrictMode>
        <QueryClientProvider client={queryClient}>
          <Host myExtension={MY_EXTENSION} />
        </QueryClientProvider>
      </StrictMode>,
    )
    elapse(0)
    expect(msUntil('ABSENT')).toBe(FIRST_HELLO_TIMEOUT_MS)
  })

  it('concludes nothing for a page that signs out while it waits', () => {
    installBackend()
    vi.useFakeTimers()
    renderBridge()
    elapse(0)
    act(() => bridge.deprovision())
    elapse(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS)
    expect(bridge.presence).toBe('UNKNOWN')
  })

  // An install, an update or a site allowed while the page is open marks it
  // late. Whatever was concluded before, the marker turns it back into a
  // question, and the ordinary run asks it.
  describe.each([
    ['inside the first deadline', 300],
    ['after the page had read absent', 3000],
  ])('a marker that lands %s', (_case, landsAt) => {
    function markLate() {
      installBackend()
      vi.useFakeTimers()
      renderBridge()
      elapse(0)
      elapse(landsAt)
      expect(bridge.presence).toBe(landsAt > FIRST_HELLO_TIMEOUT_MS ? 'ABSENT' : 'UNKNOWN')
      readings = []
      return act(async () => {
        document.documentElement.dataset.webSipPhone = '1'
      })
    }

    it('is unknown, then present on a hello reply', async () => {
      await markLate()
      expect(bridge.presence).toBe('UNKNOWN')
      answerLastHello()
      expect(path(readings)).toEqual(['UNKNOWN', 'PRESENT'])
      // The short deadline went with the question it was for.
      elapse(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS)
      expect(bridge.presence).toBe('PRESENT')
    })

    it('is unknown, then present on a state alone', async () => {
      await markLate()
      expect(bridge.presence).toBe('UNKNOWN')
      act(() => fromExtension({ type: 'state', ...extensionStateFixture() }))
      expect(path(readings)).toEqual(['UNKNOWN', 'PRESENT'])
      elapse(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS)
      expect(bridge.presence).toBe('PRESENT')
    })

    it('is lost only after the whole run if nothing answers, and never absent', async () => {
      await markLate()
      expect(msUntil('LOST', { step: 10 })).toBe(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS)
      expect(path(readings)).toEqual(['UNKNOWN', 'LOST'])
    })
  })

  // A tab woken from sleep runs its timers late. Its content script is there
  // (the marker says so) and may answer at any moment, so it is given the
  // whole run, however long the run's ticks take.
  it('gives a marked page the whole run when its timers run late', () => {
    installBackend()
    document.documentElement.dataset.webSipPhone = '1'
    vi.useFakeTimers()
    const LATE_MS = 1500
    const onTime = { setInterval: globalThis.setInterval, setTimeout: globalThis.setTimeout }
    const late = [
      vi.spyOn(globalThis, 'setInterval').mockImplementation(((handler: () => void, ms?: number) =>
        onTime.setInterval(handler, (ms ?? 0) + LATE_MS)) as typeof setInterval),
      vi.spyOn(globalThis, 'setTimeout').mockImplementation(((handler: () => void, ms?: number) =>
        onTime.setTimeout(handler, (ms ?? 0) + LATE_MS)) as typeof setTimeout),
    ]
    try {
      renderBridge()
      expect(msUntil('LOST', { step: 10, cap: 30_000 })).toBe(
        (HELLO_TIMEOUT_MS + LATE_MS) * HELLO_ATTEMPTS,
      )
      expect(path(readings)).toEqual(['UNKNOWN', 'LOST'])
    } finally {
      for (const spy of late) spy.mockRestore()
    }
  })

  // Issue #187: a content script that answers late inside its first attempt
  // is present, and nothing else is ever read in between.
  it('is present, and nothing else, for a marked page that answers 1.9 s in', () => {
    installBackend()
    document.documentElement.dataset.webSipPhone = '1'
    vi.useFakeTimers()
    renderBridge()
    elapse(1900)
    expect(hellosPosted()).toBe(1)
    answerLastHello()
    elapse(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS * 2)
    expect(path(readings)).toEqual(['UNKNOWN', 'PRESENT'])
  })

  // A background tab runs its timers (Chrome only spaces them out), so it
  // has its verdict before anyone looks. Showing it asks again, as always,
  // without taking the verdict back.
  it.each([200, 3000])('is absent by the time a tab loaded in the background is shown %i ms later', (shownAt) => {
    installBackend()
    const tab = hiddenTab()
    vi.useFakeTimers()
    renderBridge()
    elapse(0)
    elapse(shownAt)
    tab.show()
    expect(msUntil('ABSENT')).toBeLessThanOrEqual(FIRST_HELLO_TIMEOUT_MS)
    elapse(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS)
    expect(bridge.presence).toBe('ABSENT')
  })
})

describe('when the bridge stops having a phone', () => {
  function posted(type: string) {
    return postMessage.mock.calls.filter((call) => (call[0] as { type?: string }).type === type)
  }

  /** A bridge whose `enabled` the test can change, optionally under StrictMode. */
  function renderToggle(initial: boolean, { strict = false } = {}) {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
    })
    const tree = (enabled: boolean) => {
      const inner = (
        <QueryClientProvider client={queryClient}>
          <Host enabled={enabled} myExtension={MY_EXTENSION} />
        </QueryClientProvider>
      )
      return strict ? <StrictMode>{inner}</StrictMode> : inner
    }
    const view = render(tree(initial))
    return { ...view, setEnabled: (enabled: boolean) => view.rerender(tree(enabled)) }
  }

  it('deprovisions once when enabled goes false', () => {
    installBackend()
    vi.useFakeTimers()
    const view = renderToggle(true)
    view.setEnabled(false)
    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(posted('deprovision')).toHaveLength(1)
  })

  it('says nothing for a bridge that was never enabled, or that did not change', () => {
    installBackend()
    vi.useFakeTimers()
    const view = renderToggle(false)
    view.setEnabled(false)
    view.unmount()
    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(posted('deprovision')).toHaveLength(0)

    const second = renderToggle(false)
    second.setEnabled(true)
    second.setEnabled(true)
    act(() => {
      vi.advanceTimersByTime(1)
    })
    expect(posted('deprovision')).toHaveLength(0)
  })

  it('deprovisions once when it unmounts while enabled', () => {
    installBackend()
    vi.useFakeTimers()
    const view = renderToggle(true)
    view.unmount()
    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(posted('deprovision')).toHaveLength(1)
  })

  it('does not mistake StrictMode simulated unmount for a real one', () => {
    installBackend()
    vi.useFakeTimers()
    const view = renderToggle(true, { strict: true })
    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(posted('deprovision')).toHaveLength(0)
    expect(posted('hello').length).toBeGreaterThan(0)
    view.unmount()
    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(posted('deprovision')).toHaveLength(1)
  })

  it('stays quiet after an explicit deprovision until it is enabled again', () => {
    installBackend()
    vi.useFakeTimers()
    const view = renderToggle(true)
    act(() => bridge.deprovision())
    const sent = postMessage.mock.calls.length

    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
      bridge.sendHello()
      bridge.provision(sipSessionFixture())
      vi.advanceTimersByTime(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS + HELLO_RECHECK_MS)
    })
    expect(postMessage.mock.calls.length).toBe(sent)

    // The session clearing is the stop the deprovision already announced.
    view.setEnabled(false)
    act(() => {
      vi.advanceTimersByTime(1000)
    })
    expect(posted('deprovision')).toHaveLength(1)

    view.setEnabled(true)
    expect(posted('hello').length).toBeGreaterThan(0)
    expect(postMessage.mock.calls.length).toBeGreaterThan(sent)
  })
})
