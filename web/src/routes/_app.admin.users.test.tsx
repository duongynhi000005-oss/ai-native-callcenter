import { screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactElement } from 'react'

import type { User } from '@/lib/users'
import { renderWithProviders } from '@/test/harness'

import { Route } from './_app.admin.users'

/**
 * The accounts screen and the call-taking account that has no phone (#268).
 * Such an agent cannot sign in; saving the account unchanged is what gives it
 * one, and the screen offers exactly that on exactly those rows.
 */

const Users = Route.options.component as () => ReactElement

const id = (n: number) => `00000000-0000-4000-8000-0000000000${n.toString().padStart(2, '0')}`

function userFixture(over: Partial<User>): User {
  return {
    userId: id(1),
    username: 'amy',
    displayName: 'Amy',
    role: 'AGENT',
    status: 'ACTIVE',
    ...over,
  }
}

const ACCOUNTS: User[] = [
  // An agent whose identity never got a phone.
  userFixture({ userId: id(1), username: 'amy', displayName: 'Amy Lin', agentId: id(11),
    callcenterName: 'agent-amy' }),
  // A suspended supervisor in the same state.
  userFixture({ userId: id(2), username: 'cara', displayName: '王卡拉', role: 'SUPERVISOR',
    status: 'SUSPENDED', agentId: id(12), callcenterName: 'agent-cara' }),
  // An administrator: no phone, and none to give.
  userFixture({ userId: id(3), username: 'root', displayName: 'Root', role: 'ADMIN' }),
  // An agent who has one.
  userFixture({ userId: id(4), username: 'wei', displayName: 'Wei', agentId: id(14),
    callcenterName: 'agent-wei', extensionNumber: '1003' }),
]

interface Recorded {
  method: string
  path: string
  body: unknown
}

const json = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

/** The users endpoints, giving a phone on update the way the server does. */
function stubUsers(accounts: User[], { isPoolExhausted = false } = {}) {
  const rows = accounts.map((a) => ({ ...a }))
  const requests: Recorded[] = []
  let nextNumber = 1004
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const path = String(input).replace('/api/v1', '')
      const method = init.method ?? 'GET'
      const body = typeof init.body === 'string' ? JSON.parse(init.body) : undefined
      requests.push({ method, path, body })
      if (path === '/users' && method === 'GET') return json({ items: rows })
      if (path.startsWith('/users/') && method === 'PUT') {
        if (isPoolExhausted) {
          return json({ error: { code: 'EXTENSION_POOL_EXHAUSTED', message: 'no numbers' } }, 409)
        }
        const row = rows.find((r) => r.userId === path.slice('/users/'.length))!
        Object.assign(row, body)
        if (row.role !== 'ADMIN' && !row.extensionNumber) row.extensionNumber = String(nextNumber++)
        return json(row)
      }
      return json({ items: [] })
    }),
  )
  return requests
}

afterEach(() => {
  vi.unstubAllGlobals()
})

const rowOf = (username: string) => screen.getByText(username).closest('tr') as HTMLElement

describe('assigning a phone', () => {
  it('is offered on a call-taking account with no phone, and nowhere else', async () => {
    stubUsers(ACCOUNTS)
    renderWithProviders(<Users />)
    await screen.findByText('amy')

    const offers = screen.getAllByRole('button', { name: 'Assign a phone' })
    expect(offers).toHaveLength(2)
    expect(within(rowOf('amy')).getByRole('button', { name: 'Assign a phone' })).toBeInTheDocument()
    expect(within(rowOf('cara')).getByRole('button', { name: 'Assign a phone' })).toBeInTheDocument()
    // An administrator takes no calls; an agent with a phone needs none.
    expect(within(rowOf('root')).queryByRole('button', { name: 'Assign a phone' })).not.toBeInTheDocument()
    expect(within(rowOf('wei')).queryByRole('button', { name: 'Assign a phone' })).not.toBeInTheDocument()
    expect(within(rowOf('wei')).getByText('1003')).toBeInTheDocument()
  })

  it('saves the account unchanged, and shows the phone the server gave', async () => {
    const requests = stubUsers(ACCOUNTS)
    const { user } = renderWithProviders(<Users />)
    await screen.findByText('cara')

    await user.click(within(rowOf('cara')).getByRole('button', { name: 'Assign a phone' }))

    await waitFor(() => expect(requests.some((r) => r.method === 'PUT')).toBe(true))
    const puts = requests.filter((r) => r.method !== 'GET')
    expect(puts).toEqual([
      {
        method: 'PUT',
        path: `/users/${id(2)}`,
        // Exactly what the row reads: nothing about the account changes.
        body: { username: 'cara', displayName: '王卡拉', role: 'SUPERVISOR', status: 'SUSPENDED' },
      },
    ])
    expect(await within(rowOf('cara')).findByText('1004')).toBeInTheDocument()
    expect(within(rowOf('cara')).queryByRole('button', { name: 'Assign a phone' })).not.toBeInTheDocument()
    // The other phoneless account is still waiting for its own.
    expect(within(rowOf('amy')).getByRole('button', { name: 'Assign a phone' })).toBeInTheDocument()
  })

  it('says so on the row when the pool has no number left', async () => {
    stubUsers(ACCOUNTS, { isPoolExhausted: true })
    const { user } = renderWithProviders(<Users />)
    await screen.findByText('amy')

    await user.click(within(rowOf('amy')).getByRole('button', { name: 'Assign a phone' }))

    expect(await within(rowOf('amy')).findByRole('alert')).toHaveTextContent(
      'There are no free extension numbers left in the pool.',
    )
    expect(within(rowOf('cara')).queryByRole('alert')).not.toBeInTheDocument()
  })
})
