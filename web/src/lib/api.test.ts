import { afterEach, describe, expect, it, vi } from 'vitest'

import { ApiError, callApi } from './api'

const CALL_ID = '019a0000-0000-7000-8000-000000000221'

function reply(status: number, body: unknown) {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () =>
        new Response(JSON.stringify(body), {
          status,
          headers: { 'content-type': 'application/json' },
        }),
    ),
  )
}

afterEach(() => {
  vi.unstubAllGlobals()
})

// createCall answers 202 when the switch has not confirmed the placement: the
// phone may still ring, so click-to-dial must take it as placed, not show an
// error (#221).
describe('click-to-dial', () => {
  it('treats 202 like 201: the call is placed, not failed', async () => {
    reply(202, { callId: CALL_ID })
    await expect(callApi.dial('18688886666')).resolves.toEqual({ callId: CALL_ID })
  })

  it('still rejects SWITCH_DOWN', async () => {
    reply(503, { error: { code: 'SWITCH_DOWN', message: 'the switch is unreachable' } })
    await expect(callApi.dial('18688886666')).rejects.toBeInstanceOf(ApiError)
  })
})
