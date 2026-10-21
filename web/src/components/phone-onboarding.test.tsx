import { act, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { PhoneOnboarding } from '@/components/phone-onboarding'
import {
  FIRST_HELLO_TIMEOUT_MS, HELLO_ATTEMPTS, HELLO_RECHECK_MS, HELLO_TIMEOUT_MS, PhoneBridgeProvider,
  usePhoneBridgeValue, type PhoneBridge,
} from '@/lib/phone-bridge'
import {
  installBackend, installFakeExtension, renderWithProviders, type FakeExtension,
} from '@/test/harness'

/**
 * The setup card.
 *
 * Every row reads the extension's own state, so nothing here is a checkbox
 * the agent ticks: installing it completes step one, and the extension
 * answering at all completes step two, because a content script only runs on
 * a site its owner allowed. The card leaves when the last row completes and
 * nothing else closes it.
 */

afterEach(() => {
  vi.unstubAllGlobals()
  vi.unstubAllEnvs()
  delete document.documentElement.dataset.webSipPhone
  vi.useRealTimers()
})

/**
 * The card opens on a settled reading, never on the first guess: until a run
 * of hellos has been answered or has gone unanswered to its last attempt the
 * page does not know, and shows nothing (issue #187). Only the interval is
 * faked, so the rest of the page keeps its real clock.
 */
function fakeTheIntervals() {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
}

/** Every hello of the run unanswered; the intervals must be faked before mount. */
function letTheHellosRunOut() {
  act(() => {
    vi.advanceTimersByTime(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS)
  })
}

/** A card for an agent whose hellos have gone unanswered with no marker. */
function renderAbsent(options: { isExtensionInstalled?: boolean } = {}) {
  fakeTheIntervals()
  const view = renderCard(options)
  letTheHellosRunOut()
  return view
}

/** The bridge behind the card, so a test can ask for the card itself. */
let bridge: PhoneBridge

function Card() {
  const phone = usePhoneBridgeValue(true)
  bridge = phone
  return (
    <PhoneBridgeProvider value={phone}>
      <PhoneOnboarding />
    </PhoneBridgeProvider>
  )
}

function renderCard(options: { isExtensionInstalled?: boolean } = {}) {
  installBackend(options)
  return renderWithProviders(<Card />)
}

/**
 * The content script stays (its marker is on the page) but stops answering,
 * and a run of hellos goes unanswered to its last attempt.
 */
async function goSilent(fake: FakeExtension) {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
  fake.silence()
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'))
  })
  act(() => {
    vi.advanceTimersByTime(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS)
  })
}

/** The status word beside each step title, in order. `Checking` is neither. */
function steps() {
  return screen.getAllByRole('listitem').map((item) => ({
    title: item.textContent ?? '',
    isDone: (item.textContent ?? '').includes('Done'),
    isChecking: (item.textContent ?? '').includes('Checking'),
  }))
}

describe('an agent with no extension', () => {
  // An earlier install on this origin is not a reason to say "reload": with
  // no content script in the tab there is nothing to reload into (issue #75).
  it('is shown the wizard at first paint despite a stale memory of one', async () => {
    window.localStorage.setItem('aicc.phone.extensionSeen', '1')
    renderAbsent()
    expect(await screen.findByText(/set up your phone/i)).toBeInTheDocument()
    expect(screen.queryByText(/lost contact with this page/i)).toBeNull()
    window.localStorage.removeItem('aicc.phone.extensionSeen')
  })

  // Nothing has been seen here to lose contact with, so it is told to install.
  it('is shown the card, blocking, with nothing done', async () => {
    renderAbsent()
    expect(await screen.findByText(/set up your phone/i)).toBeInTheDocument()
    expect(steps().map((s) => s.isDone)).toEqual([false, false, false])
    expect(screen.queryByText(/lost contact with this page/i)).toBeNull()
  })

  /**
   * A link into an extension is only as good as the id in it. Chrome blocks
   * `chrome-extension://replace_with_web_store_id/…` outright — that shipped
   * and an agent hit ERR_BLOCKED_BY_CLIENT — so with no id to address, the
   * card says how to get there by hand instead of offering a dead button.
   */
  it('addresses the published extension when the build was told nothing', async () => {
    // `web/.env.local` may say otherwise on a developer's machine, so the
    // test states the case it is about: no override, the published id.
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', '')
    renderAbsent()
    await screen.findByText(/set up your phone/i)
    expect(screen.getByRole('link', { name: /web store/i })).toHaveAttribute(
      'href',
      'https://chromewebstore.google.com/detail/dkhaojcfjdcdpldokeokajkmambkbacp',
    )
    expect(document.body.innerHTML).toContain('chrome-extension://dkhaojcfjdcdpldokeokajkmambkbacp/')
  })

  it('uses the id this build was given, when it was given one', async () => {
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', 'abcdefghijklmnopabcdefghijklmnop')
    renderAbsent()
    await screen.findByText(/set up your phone/i)
    expect(screen.getByRole('link', { name: /web store/i })).toHaveAttribute(
      'href',
      'https://chromewebstore.google.com/detail/abcdefghijklmnopabcdefghijklmnop',
    )
    expect(screen.getByRole('link', { name: /microphone settings/i })).toHaveAttribute(
      'href',
      'chrome-extension://abcdefghijklmnopabcdefghijklmnop/options.html#microphone',
    )
    // A page the agent opens themselves, in their own tab.
    const store = screen.getByRole('link', { name: /web store/i })
    expect(store).toHaveAttribute('target', '_blank')
    expect(store).toHaveAttribute('rel', expect.stringContaining('noopener'))
  })

  // The site step's link is offered once step 1 is done: with the extension
  // installed it is the options page, with this build's id in it.
  it('links the site step into the extension the build was told about', async () => {
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', 'abcdefghijklmnopabcdefghijklmnop')
    renderAbsent({ isExtensionInstalled: true })
    expect(await screen.findByRole('link', { name: /extension options/i })).toHaveAttribute(
      'href',
      `chrome-extension://abcdefghijklmnopabcdefghijklmnop/options.html?site=${window.location.hostname}`,
    )
  })

  it('cannot dismiss it with Escape', async () => {
    renderAbsent()
    await screen.findByText(/set up your phone/i)
    await userEvent.keyboard('{Escape}')
    expect(screen.getByText(/set up your phone/i)).toBeInTheDocument()
  })
})

describe('an extension installed but not allowed on this site', () => {
  // Nothing answers hello, because no content script runs here; the options
  // page the manifest exposes is what proves the install (issue #81).
  it('ticks the install and leaves the site step pending', async () => {
    renderAbsent({ isExtensionInstalled: true })
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    expect(steps().map((s) => s.isDone)).toEqual([true, false, false])
  })

  it('ticks the install when the agent comes back to the tab after installing', async () => {
    const backend = installBackend()
    fakeTheIntervals()
    renderWithProviders(<Card />)
    letTheHellosRunOut()
    expect(await screen.findByText(/set up your phone/i)).toBeInTheDocument()
    expect(steps()[0].isDone).toBe(false)
    backend.isExtensionInstalled = true
    act(() => {
      window.dispatchEvent(new Event('focus'))
    })
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    expect(steps()[1].isDone).toBe(false)
  })

  // A differently keyed build misses the probe; its answering is still proof.
  it('ticks the install on an answer alone when the probe finds nothing', async () => {
    const fake = installFakeExtension({ state: { microphone: 'DENIED' } })
    renderCard({ isExtensionInstalled: false })
    await waitFor(() => expect(steps()[1].isDone).toBe(true))
    expect(steps().map((s) => s.isDone)).toEqual([true, true, false])
    fake.uninstall()
  })
})

describe('as the agent works through it', () => {
  let extension: FakeExtension | undefined

  afterEach(async () => {
    // An uninstall removes the marker, which the mounted card reacts to.
    await act(async () => extension?.uninstall())
    extension = undefined
  })

  // Step two has no signal of its own: being answered is the signal. The
  // microphone step waits for a report, so an extension that has not sent one
  // yet leaves the card up.
  it.each([
    ['reports no microphone', { microphone: 'DENIED' as const }],
    ['has not reported yet', null],
  ])('completes install and site, and stays, for an extension that %s', async (_case, state) => {
    extension = installFakeExtension({ state })
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    expect(steps().map((s) => s.isDone)).toEqual([true, true, false])
    expect(screen.getByText(/set up your phone/i)).toBeInTheDocument()
  })

  it('completes the microphone when the extension reports it granted, and leaves', async () => {
    const fake = installFakeExtension({ state: { microphone: 'DENIED' } })
    extension = fake
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    await act(async () => {
      fake.report({ microphone: 'GRANTED' })
    })
    await waitFor(() => expect(screen.queryByText(/set up your phone/i)).toBeNull())
  })

  // The id an unpacked build has is one no bundle could have carried, so the
  // extension announcing its own is the only way to link into it.
  it('links into the extension by the id it announced for itself', async () => {
    // It outranks the build-time id: an unpacked install has an id of its own
    // and the bundle's is at best the last machine's.
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', 'abcdefghijklmnopabcdefghijklmnop')
    extension = installFakeExtension({
      extensionId: 'ponmlkjihgfedcbaponmlkjihgfedcba',
      state: { microphone: 'DENIED' },
    })
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    await waitFor(() =>
      expect(screen.getByRole('link', { name: /microphone settings/i })).toHaveAttribute(
        'href',
        'chrome-extension://ponmlkjihgfedcbaponmlkjihgfedcba/options.html#microphone',
      ),
    )
  })

  it('ignores an announced id that is not one and falls back to the published id', async () => {
    vi.stubEnv('VITE_WEB_SIP_PHONE_ID', '')
    extension = installFakeExtension({ extensionId: 'nope', state: { microphone: 'DENIED' } })
    renderCard()
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    expect(document.body.innerHTML).not.toContain('chrome-extension://nope/')
    expect(document.body.innerHTML).toContain('chrome-extension://dkhaojcfjdcdpldokeokajkmambkbacp/')
  })

  it('offers a reload on the site step for builds that do not inject into open tabs', async () => {
    renderAbsent({ isExtensionInstalled: true })
    await screen.findByText(/set up your phone/i)
    expect(await screen.findByRole('button', { name: /reload after allowing/i })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /reload after installing/i })).toBeNull()
  })

  it('offers a reload on the install step for builds that do not inject into open tabs', async () => {
    renderAbsent()
    await screen.findByText(/set up your phone/i)
    expect(screen.getByRole('button', { name: /reload after installing/i })).toBeInTheDocument()
    // Nothing to allow until there is something installed.
    expect(screen.queryByRole('button', { name: /reload after allowing/i })).toBeNull()
  })

  it('ticks and leaves for an extension injected into the open tab, without a reload', async () => {
    renderAbsent()
    await screen.findByText(/set up your phone/i)
    const fake = installFakeExtension({ state: { microphone: 'DENIED' } })
    extension = fake
    await act(async () => {
      fake.mark()
    })
    await waitFor(() => expect(steps()[1].isDone).toBe(true))
    expect(steps()[0].isDone).toBe(true)
    await act(async () => {
      fake.report({ microphone: 'GRANTED' })
    })
    await waitFor(() => expect(screen.queryByText(/set up your phone/i)).toBeNull())
  })

  /**
   * The extension holds the registration in a worker that survives the
   * machine sleeping; the content script in the sleeping tab may stop
   * answering while its marker stays. The phone is installed, allowed and registered
   * — three undone steps would be three lies, and the agent would go looking
   * for a Web Store page to reinstall what they already have. The one true
   * thing is that this page cannot reach it, and the one cure is a reload.
   */
  it('says it lost contact, not that nothing is installed, when the marker stays and nothing answers', async () => {
    const fake = installFakeExtension({ state: { microphone: 'GRANTED' } })
    extension = fake
    fake.mark()
    renderCard()
    await waitFor(() => expect(screen.queryByText(/set up your phone/i)).toBeNull())
    await goSilent(fake)
    expect(await screen.findByText(/lost contact with this page/i)).toBeInTheDocument()
    expect(screen.queryByText(/set up your phone/i)).toBeNull()
    expect(screen.queryByRole('listitem')).toBeNull()

    const reload = vi.fn()
    vi.stubGlobal('location', { ...window.location, reload })
    await userEvent.click(screen.getByRole('button', { name: /reload the page/i }))
    expect(reload).toHaveBeenCalledTimes(1)
  })

  // Asking for the card is asking for the steps: an agent who opens it
  // themselves wants the links into the extension, whatever the page can
  // currently reach.
  it('still shows the three steps when the agent opens the card themselves', async () => {
    const fake = installFakeExtension({ state: { microphone: 'GRANTED' } })
    extension = fake
    fake.mark()
    renderCard()
    await waitFor(() => expect(screen.queryByText(/set up your phone/i)).toBeNull())
    await goSilent(fake)
    await screen.findByText(/lost contact with this page/i)
    await act(async () => {
      bridge.openOnboarding()
    })
    expect(await screen.findByText(/set up your phone/i)).toBeInTheDocument()
    expect(steps()).toHaveLength(3)
  })
})

/**
 * What the card does with each reading of the extension (issue #187): it asks
 * nothing of an agent on a guess, and it asks only for what is undone.
 */
describe('the card for each reading of the extension', () => {
  let extension: FakeExtension | undefined

  afterEach(async () => {
    await act(async () => extension?.uninstall())
    extension = undefined
  })

  // The first hello has not been answered and no run has run out: the page
  // does not know, so it says nothing, however long the answer takes inside
  // the run.
  it('renders nothing while the first hellos are still being asked', async () => {
    // A marked page: an unmarked one is given a short first run instead.
    document.documentElement.dataset.webSipPhone = '1'
    fakeTheIntervals()
    renderCard({ isExtensionInstalled: true })
    expect(bridge.presence).toBe('UNKNOWN')
    expect(screen.queryByText(/set up your phone/i)).toBeNull()
    act(() => {
      vi.advanceTimersByTime(HELLO_TIMEOUT_MS * (HELLO_ATTEMPTS - 1))
    })
    expect(bridge.presence).toBe('UNKNOWN')
    expect(screen.queryByText(/set up your phone/i)).toBeNull()
    expect(screen.queryByText(/lost contact with this page/i)).toBeNull()
  })

  it('shows install pending, and nothing to allow yet, when nothing is installed', async () => {
    renderAbsent({ isExtensionInstalled: false })
    await screen.findByText(/set up your phone/i)
    expect(steps().map((s) => s.isDone)).toEqual([false, false, false])
    expect(screen.getByRole('link', { name: /web store/i })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /extension options/i })).toBeNull()
  })

  it('shows install done and the site step pending, with the site named, when installed', async () => {
    renderAbsent({ isExtensionInstalled: true })
    await waitFor(() => expect(steps()[0].isDone).toBe(true))
    expect(steps().map((s) => s.isDone)).toEqual([true, false, false])
    expect(steps()[1].isChecking).toBe(false)
    expect(screen.getByText(new RegExp(`Add ${window.location.hostname}`))).toBeInTheDocument()
    expect(await screen.findByRole('link', { name: /extension options/i })).toBeInTheDocument()
  })

  // The extension is answering, so steps 1 and 2 are true; only the
  // microphone is undone.
  it('shows only the microphone undone for an extension that is there and denied', async () => {
    extension = installFakeExtension({ state: { microphone: 'DENIED' } })
    renderCard()
    await waitFor(() => expect(steps()[1].isDone).toBe(true))
    expect(steps().map((s) => s.isDone)).toEqual([true, true, false])
  })

  it('shows the lost card, not the steps, for a marker that stays and a silent extension', async () => {
    document.documentElement.dataset.webSipPhone = '1'
    fakeTheIntervals()
    renderCard()
    letTheHellosRunOut()
    expect(bridge.presence).toBe('LOST')
    expect(await screen.findByText(/lost contact with this page/i)).toBeInTheDocument()
    expect(screen.queryByText(/set up your phone/i)).toBeNull()
  })

  // Opened by the agent while the page does not know yet, the site step is
  // not a task and not a failure either.
  it('shows the site step as checking when the agent opens the card while nothing is settled', async () => {
    // Marked, so nothing settles before the whole run does.
    document.documentElement.dataset.webSipPhone = '1'
    fakeTheIntervals()
    renderCard({ isExtensionInstalled: true })
    await act(async () => {
      bridge.openOnboarding()
    })
    await screen.findByText(/set up your phone/i)
    expect(bridge.presence).toBe('UNKNOWN')
    expect(steps()[1].isChecking).toBe(true)
    expect(steps()[1].isDone).toBe(false)
    expect(screen.queryByRole('button', { name: /reload after allowing/i })).toBeNull()
  })

  // Replacing the extension removes its marker and writes another a task
  // later. The wizard must not flash up in between for a phone that is fine.
  it('keeps steps 1 and 2 done through a marker removal while the microphone is denied', async () => {
    const fake = installFakeExtension({ state: { microphone: 'DENIED' } })
    extension = fake
    fake.mark()
    renderCard()
    await waitFor(() => expect(steps()[1].isDone).toBe(true))
    await act(async () => {
      delete document.documentElement.dataset.webSipPhone
    })
    expect(steps().map((s) => s.isDone)).toEqual([true, true, false])
    await act(async () => {
      fake.mark()
    })
    expect(steps().map((s) => s.isDone)).toEqual([true, true, false])
  })

  // The card that came up on silence leaves by itself when the content
  // script returns: nobody has to find the Reload button.
  it('goes away by itself when a silent extension starts answering at a recheck', async () => {
    fakeTheIntervals()
    const fake = installFakeExtension({ state: { microphone: 'GRANTED' }, answersHello: false })
    extension = fake
    renderCard()
    letTheHellosRunOut()
    await screen.findByText(/set up your phone/i)
    fake.silence()
    extension = installFakeExtension({ state: { microphone: 'GRANTED' } })
    act(() => {
      vi.advanceTimersByTime(HELLO_RECHECK_MS)
    })
    await waitFor(() => expect(screen.queryByText(/set up your phone/i)).toBeNull())
    expect(bridge.presence).toBe('PRESENT')
  })
})

function elapse(ms: number) {
  act(() => {
    vi.advanceTimersByTime(ms)
  })
}

const isCardUp = () => screen.queryByText(/set up your phone/i) !== null
const isLostUp = () => screen.queryByText(/lost contact with this page/i) !== null

/** One message from the extension, as the content script posts it. */
function fromExtension(data: Record<string, unknown>) {
  act(() => {
    window.dispatchEvent(
      new MessageEvent('message', {
        data: { source: 'web-sip-phone', protocolVersion: 1, ...data },
        origin: window.location.origin,
        source: window,
      }),
    )
  })
}

function lastHelloNonce(postMessage: ReturnType<typeof vi.spyOn>): string {
  const hellos = postMessage.mock.calls.filter((call) => (call[0] as { type?: string }).type === 'hello')
  return (hellos[hellos.length - 1][0] as { nonce: string }).nonce
}

const grantedState = {
  registration: 'REGISTERED',
  account: '1001',
  sipDomain: null,
  credentialSource: 'PROVISIONED',
  microphone: 'GRANTED',
  error: null,
}

/**
 * How soon the card comes up for an agent without the extension (issue #292).
 *
 * A page no content script has marked by the time it has loaded is told so
 * after one short hello, and the card opens then, with step 1 already
 * showing what the install probe found. A marked page keeps the whole run,
 * so the card never flashes at an agent whose phone is only slow to answer.
 * Every clock is faked here, so each assertion reads the card as it is.
 */
describe('how soon the card comes up', () => {
  afterEach(() => {
    Reflect.deleteProperty(document, 'visibilityState')
  })

  it.each([
    ['installed but not allowed', true],
    ['not installed', false],
  ])('opens a short deadline after load, with step 1 already right, when %s', async (_case, isInstalled) => {
    vi.useFakeTimers()
    renderCard({ isExtensionInstalled: isInstalled })
    // The install probe settles while the card is still closed.
    await act(async () => {})
    elapse(0)
    elapse(FIRST_HELLO_TIMEOUT_MS - 1)
    expect(isCardUp()).toBe(false)
    elapse(1)
    expect(isCardUp()).toBe(true)
    expect(bridge.presence).toBe('ABSENT')
    // Read on the card's first frame, before anything else has happened.
    expect(steps()[0].isDone).toBe(isInstalled)
    expect(FIRST_HELLO_TIMEOUT_MS).toBeLessThan(2000)
  })

  it('opens on time with focus and tab switches fired five times while it waits', () => {
    vi.useFakeTimers()
    renderCard()
    elapse(0)
    for (let i = 0; i < 5; i += 1) {
      elapse(90)
      act(() => {
        window.dispatchEvent(new Event('focus'))
        document.dispatchEvent(new Event('visibilitychange'))
      })
    }
    elapse(FIRST_HELLO_TIMEOUT_MS - 5 * 90 - 1)
    expect(isCardUp()).toBe(false)
    elapse(1)
    expect(isCardUp()).toBe(true)
  })

  // The extension installed, updated or allowed after the page loaded: the
  // marker landing turns the silence back into a question, the card leaves,
  // and the extension's first word settles it.
  describe.each([
    ['inside the first deadline', 300],
    ['after the card came up', 3000],
  ])('a marker that lands %s', (_case, landsAt) => {
    async function markLate() {
      const postMessage = vi.spyOn(window, 'postMessage')
      vi.useFakeTimers()
      renderCard({ isExtensionInstalled: true })
      await act(async () => {})
      elapse(0)
      elapse(landsAt)
      expect(isCardUp()).toBe(landsAt > FIRST_HELLO_TIMEOUT_MS)
      await act(async () => {
        document.documentElement.dataset.webSipPhone = '1'
      })
      expect(bridge.presence).toBe('UNKNOWN')
      expect(isCardUp()).toBe(false)
      return postMessage
    }

    it('closes the card and stays closed on a hello reply', async () => {
      const postMessage = await markLate()
      fromExtension({ type: 'hello', nonce: lastHelloNonce(postMessage), extensionVersion: '1.4.0' })
      fromExtension({ type: 'state', ...grantedState })
      expect(bridge.presence).toBe('PRESENT')
      elapse(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS + HELLO_RECHECK_MS)
      expect(isCardUp()).toBe(false)
      expect(isLostUp()).toBe(false)
    })

    it('closes the card and stays closed on a state alone, with no hello reply', async () => {
      await markLate()
      fromExtension({ type: 'state', ...grantedState })
      expect(bridge.presence).toBe('PRESENT')
      elapse(HELLO_TIMEOUT_MS * HELLO_ATTEMPTS + HELLO_RECHECK_MS)
      expect(isCardUp()).toBe(false)
      expect(isLostUp()).toBe(false)
    })
  })

  // Issue #187: a marked page whose content script answers late inside its
  // first attempt never sees the card, not even for a frame.
  it('never opens for a marked page whose extension answers 1.9 s into the first hello', async () => {
    const postMessage = vi.spyOn(window, 'postMessage')
    document.documentElement.dataset.webSipPhone = '1'
    vi.useFakeTimers()
    renderCard({ isExtensionInstalled: true })
    await act(async () => {})
    for (let at = 0; at < 1900; at += 100) {
      elapse(100)
      expect(isCardUp() || isLostUp()).toBe(false)
    }
    fromExtension({ type: 'hello', nonce: lastHelloNonce(postMessage), extensionVersion: '1.4.0' })
    fromExtension({ type: 'state', ...grantedState })
    for (let at = 0; at < HELLO_TIMEOUT_MS * HELLO_ATTEMPTS * 2; at += 100) {
      elapse(100)
      expect(isCardUp() || isLostUp()).toBe(false)
    }
  })

  // A tab opened in the background has had its verdict before anyone looks,
  // so the card is there the moment it is first shown, or very soon after.
  it.each([200, 3000])('is up within the deadline of a background tab being shown %i ms after load', (shownAt) => {
    let visibilityState: DocumentVisibilityState = 'hidden'
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibilityState })
    vi.useFakeTimers()
    renderCard()
    elapse(0)
    elapse(shownAt)
    act(() => {
      visibilityState = 'visible'
      document.dispatchEvent(new Event('visibilitychange'))
    })
    let waited = 0
    while (!isCardUp() && waited < FIRST_HELLO_TIMEOUT_MS) {
      elapse(1)
      waited += 1
    }
    expect(isCardUp()).toBe(true)
    if (shownAt >= FIRST_HELLO_TIMEOUT_MS) expect(waited).toBe(0)
  })
})
