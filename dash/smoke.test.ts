// Renders the real App against a stubbed API in a DOM, so a broken import or a misused
// Kumo prop fails here rather than in the browser.

import { afterEach, expect, mock, test } from 'bun:test'
import { act } from 'react'
import { createElement } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { Toasty } from '@cloudflare/kumo'
import { App } from './app'

const SITES = [
  {
    slug: 'a3f9c1d2e4b60718',
    title: 'Quarterly report',
    src: 'report.html',
    created: '2026-08-02T06:31:34Z',
    expires: '',
    files: 1,
    bytes: 37204,
    access: 'link',
  },
  {
    slug: 'demo',
    title: '',
    src: 'site',
    created: '2026-08-01T10:00:00Z',
    expires: '2026-09-01T10:00:00Z',
    files: 4,
    bytes: 1048576,
    access: 'auth',
  },
]

let root: Root | null = null
let host: HTMLElement | null = null

afterEach(() => {
  act(() => root?.unmount())
  host?.remove()
  root = null
  host = null
})

async function render(payload: unknown, status = 200) {
  const calls: { path: string; body: unknown }[] = []
  globalThis.fetch = mock(async (input: string, init: RequestInit) => {
    calls.push({ path: String(input), body: JSON.parse(String(init.body)) })
    return { ok: status < 400, status, json: async () => payload } as Response
  }) as unknown as typeof fetch

  host = document.createElement('div')
  document.body.append(host)
  root = createRoot(host)

  await act(async () => {
    root!.render(createElement(Toasty, null, createElement(App)))
  })
  return { calls, text: () => host!.textContent ?? '' }
}

test('renders the site table from the API', async () => {
  const { calls, text } = await render({
    sites: SITES,
    totals: { sites: 2, files: 5, bytes: 1085780 },
  })

  expect(calls.map((c) => c.path)).toEqual(['/api/list'])

  expect(text()).toContain('a3f9c1d2e4b60718')
  expect(text()).toContain('Quarterly report')
  expect(text()).toContain('2 sites')
  expect(text()).toContain('1.0 MB of 1 GB')
  // BASE came from the injected global, not hardcoded
  expect(text()).toContain('files on example.com')

  // one badge per site, reflecting its access mode
  expect(text()).toContain('signed link')
  expect(text()).toContain('password')

  const rows = host!.querySelectorAll('tbody tr')
  expect(rows.length).toBe(2)

  // link actions only exist for link-capable sites
  expect(rows[0].textContent).toContain('7d')
  expect(rows[1].textContent).not.toContain('7d')

  // the slug links to the published site
  expect(host!.querySelector('tbody a')?.getAttribute('href')).toBe(
    'https://a3f9c1d2e4b60718.example.com/'
  )

  // expiry is surfaced where present
  expect(rows[1].textContent).toContain('expires 2026-09-01')
})

test('shows an empty state with nothing published', async () => {
  const { text } = await render({ sites: [], totals: { sites: 0, files: 0, bytes: 0 } })
  expect(text()).toContain('Nothing published yet')
  expect(text()).toContain('sendup publish index.html')
  expect(host!.querySelector('tbody')).toBeNull()
})

test('surfaces an API failure instead of rendering blank', async () => {
  const { text } = await render({ error: 'boom' }, 500)
  expect(text()).toContain('Could not load sites')
  expect(text()).toContain('boom')
  expect(text()).toContain('Retry')
  expect(host!.querySelector('tbody')).toBeNull()
})

test('deleting a site confirms first, then posts', async () => {
  const { calls, text } = await render({
    sites: SITES,
    totals: { sites: 2, files: 5, bytes: 1085780 },
  })

  const trigger = host!.querySelector('button[aria-label="Delete a3f9c1d2e4b60718"]') as HTMLElement
  expect(trigger).not.toBeNull()

  // nothing is deleted until the dialog is confirmed
  await act(async () => trigger.click())
  expect(document.body.textContent).toContain('Delete a3f9c1d2e4b60718?')
  expect(calls.map((c) => c.path)).toEqual(['/api/list'])

  const confirm = [...document.querySelectorAll('button')].find(
    (b) => b.textContent?.trim() === 'Delete'
  ) as HTMLElement
  expect(confirm).not.toBeNull()

  await act(async () => confirm.click())
  const del = calls.find((c) => c.path === '/api/delete')
  expect(del?.body).toEqual({ slug: 'a3f9c1d2e4b60718' })

  // The row must go away from local state. Refetching instead would ask KV for a list it
  // is only eventually consistent about, and the deleted site would pop back.
  expect(host!.querySelectorAll('tbody tr').length).toBe(1)
  expect(text()).not.toContain('a3f9c1d2e4b60718')
  expect(text()).toContain('demo')
  expect(calls.filter((c) => c.path === '/api/list').length).toBe(1)

  // totals follow the removal
  expect(text()).toContain('1 site ')
})

test('each row exposes a labelled access control', async () => {
  const { calls, text } = await render({
    sites: SITES,
    totals: { sites: 2, files: 5, bytes: 1085780 },
  })
  expect(text()).toContain('signed link')

  const row = host!.querySelectorAll('tbody tr')[0]
  const select = row.querySelector('[role="combobox"]') as HTMLElement
  expect(select.getAttribute('aria-label')).toBe('Access for a3f9c1d2e4b60718')

  // only the initial list has been fetched so far
  expect(calls.map((c) => c.path)).toEqual(['/api/list'])
})
