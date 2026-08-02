// The replay layer that keeps a rename or delete visible across a page reload while
// Cloudflare KV is still catching up.

import { beforeEach, expect, test } from 'bun:test'
import { reconcile } from './app'

const site = (slug: string, files = 1, bytes = 100) =>
  ({ slug, title: '', src: '', created: '', expires: '', files, bytes, access: 'open' }) as never

const KEY = 'sendup:pending'
const setPending = (v: unknown[]) => sessionStorage.setItem(KEY, JSON.stringify(v))
const getPending = () => JSON.parse(sessionStorage.getItem(KEY) ?? '[]')

beforeEach(() => sessionStorage.clear())

test('passes the list through untouched when nothing is pending', () => {
  const r = reconcile([site('a'), site('b')])
  expect(r.sites.map((s) => s.slug)).toEqual(['a', 'b'])
  expect(r.waiting).toBe(0)
})

test('hides a deleted site the server still lists', () => {
  setPending([{ type: 'delete', slug: 'a', at: Date.now() }])
  const r = reconcile([site('a'), site('b')])
  expect(r.sites.map((s) => s.slug)).toEqual(['b'])
  expect(r.waiting).toBe(1)
  expect(getPending()).toHaveLength(1) // still outstanding
})

test('forgets a delete once the server agrees', () => {
  setPending([{ type: 'delete', slug: 'a', at: Date.now() }])
  const r = reconcile([site('b')])
  expect(r.sites.map((s) => s.slug)).toEqual(['b'])
  expect(r.waiting).toBe(0)
  expect(getPending()).toHaveLength(0)
})

test('shows the new name while the server still reports the old one', () => {
  setPending([{ type: 'rename', slug: 'old', to: 'new', at: Date.now() }])
  const r = reconcile([site('old'), site('other')])
  expect(r.sites.map((s) => s.slug)).toEqual(['new', 'other'])
  expect(r.waiting).toBe(1)
})

test('drops the stale row when mid-propagation reports both names', () => {
  setPending([{ type: 'rename', slug: 'old', to: 'new', at: Date.now() }])
  const r = reconcile([site('old'), site('new')])
  expect(r.sites.map((s) => s.slug)).toEqual(['new'])
  expect(r.waiting).toBe(1)
})

test('forgets a rename once only the new name is listed', () => {
  setPending([{ type: 'rename', slug: 'old', to: 'new', at: Date.now() }])
  const r = reconcile([site('new')])
  expect(r.sites.map((s) => s.slug)).toEqual(['new'])
  expect(r.waiting).toBe(0)
  expect(getPending()).toHaveLength(0)
})

test('expires a change that never resolved, rather than hiding a site forever', () => {
  setPending([{ type: 'delete', slug: 'a', at: Date.now() - 10 * 60_000 }])
  const r = reconcile([site('a')])
  expect(r.sites.map((s) => s.slug)).toEqual(['a'])
  expect(r.waiting).toBe(0)
})

test('survives corrupt storage instead of blanking the page', () => {
  sessionStorage.setItem(KEY, '{not json')
  const r = reconcile([site('a')])
  expect(r.sites.map((s) => s.slug)).toEqual(['a'])
})
