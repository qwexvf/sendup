// Renders the real App against a stubbed API in a DOM, so a broken import or a misused
// Kumo prop fails here rather than in the browser.

import { afterEach, beforeEach, expect, mock, test } from 'bun:test'
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

// React tracks the input's value internally, so assigning .value directly is ignored.
// Going through the native setter is what makes onChange fire.
function type(input: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!
  setter.call(input, value)
  input.dispatchEvent(new Event('input', { bubbles: true }))
}

let root: Root | null = null
let host: HTMLElement | null = null

// the app parks in-flight changes here; leaking them would skew the next test
beforeEach(() => {
  sessionStorage.clear()
  localStorage.clear()
  delete document.documentElement.dataset.mode
})

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

test('renaming posts the new subdomain and relabels the row', async () => {
  const { calls, text } = await render({
    sites: SITES,
    totals: { sites: 2, files: 5, bytes: 1085780 },
    // what /api/rename answers with
    slug: 'quarterly',
    linksInvalidated: true,
  })

  const trigger = host!.querySelector(
    'button[aria-label="Change subdomain for a3f9c1d2e4b60718"]'
  ) as HTMLElement
  expect(trigger).not.toBeNull()

  await act(async () => trigger.click())
  expect(document.body.textContent).toContain('Change subdomain')

  const input = document.querySelector('input[name="subdomain"]') as HTMLInputElement
  expect(input.value).toBe('a3f9c1d2e4b60718')

  // client-side validation rejects an invalid label without hitting the API
  await act(async () => type(input, 'Not Valid'))
  const submit = [...document.querySelectorAll('button')].find(
    (b) => b.textContent?.trim() === 'Rename'
  ) as HTMLElement
  await act(async () => submit.click())
  expect(calls.map((c) => c.path)).toEqual(['/api/list'])
  expect(document.body.textContent).toContain('Lowercase letters')

  await act(async () => type(input, 'quarterly'))
  await act(async () => submit.click())

  const req = calls.find((c) => c.path === '/api/rename')
  expect(req?.body).toEqual({ slug: 'a3f9c1d2e4b60718', to: 'quarterly' })

  // the row takes the new name locally — no refetch, same reason as delete
  expect(text()).toContain('quarterly')
  expect(text()).not.toContain('a3f9c1d2e4b60718')
  expect(calls.filter((c) => c.path === '/api/list').length).toBe(1)
  expect(host!.querySelector('tbody a')?.getAttribute('href')).toBe(
    'https://quarterly.example.com/'
  )
})

test('shows a propagation indicator while a rename is still settling', async () => {
  // a rename the server has not caught up on yet
  sessionStorage.setItem(
    'sendup:pending',
    JSON.stringify([{ type: 'rename', slug: 'a3f9c1d2e4b60718', to: 'quarterly', at: Date.now() }])
  )

  const { text } = await render({
    sites: SITES,
    totals: { sites: 2, files: 5, bytes: 1085780 },
  })

  // the corrected name survives the reload, and the delay is explained
  expect(text()).toContain('quarterly')
  expect(text()).not.toContain('a3f9c1d2e4b60718')
  expect(text()).toContain('1 change still reaching the edge')
  expect(text()).toContain('edge still catching up')
})

test('drops the indicator once the server agrees', async () => {
  sessionStorage.setItem(
    'sendup:pending',
    JSON.stringify([{ type: 'rename', slug: 'a3f9c1d2e4b60718', to: 'quarterly', at: Date.now() }])
  )

  const { text } = await render({
    sites: [{ ...SITES[0], slug: 'quarterly' }, SITES[1]],
    totals: { sites: 2, files: 5, bytes: 1085780 },
  })

  expect(text()).toContain('quarterly')
  expect(text()).not.toContain('still reaching the edge')
  expect(text()).not.toContain('edge still catching up')
  expect(sessionStorage.getItem('sendup:pending')).toBeNull()
})

test('starts dark and remembers a switch to light', async () => {
  const { text } = await render({
    sites: SITES,
    totals: { sites: 2, files: 5, bytes: 1085780 },
  })
  expect(text()).toBeTruthy()

  // the boot script sets this in the real document; unset here means the default applies
  const toLight = document.querySelector('button[aria-label="Switch to light theme"]') as HTMLElement
  expect(toLight).not.toBeNull()

  await act(async () => toLight.click())
  expect(document.documentElement.dataset.mode).toBe('light')
  expect(localStorage.getItem('sendup:mode')).toBe('light')

  const toDark = document.querySelector('button[aria-label="Switch to dark theme"]') as HTMLElement
  expect(toDark).not.toBeNull()
  await act(async () => toDark.click())
  expect(document.documentElement.dataset.mode).toBe('dark')
  expect(localStorage.getItem('sendup:mode')).toBe('dark')
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
