// Worker tests. Runs under `bun test` with no framework beyond bun:test — the Worker has
// no dependencies and neither does this.
//
// The tests share one in-memory KV and run in file order: later blocks build on state
// earlier ones set up (the dashboard section mutates sites the routing section created).

import { expect, test } from 'bun:test'
import worker from '../src/worker.js'

const BASE = 'example.com'
const at = (host: string, path = '/') => `https://${host}.${BASE}${path}`

const store = new Map<string, { v: ArrayBuffer; m: unknown }>()
const enc = (s: string) => new TextEncoder().encode(s).buffer as ArrayBuffer
const put = (k: string, v: string, m: unknown = null) => store.set(k, { v: enc(v), m })
const record = (slug: string, m: unknown = {}) => store.set(`__index:${slug}`, { v: enc(''), m })

const SITES = {
  async getWithMetadata(k: string) {
    const e = store.get(k)
    return e ? { value: e.v, metadata: e.m } : { value: null, metadata: null }
  },
  async get(k: string) {
    return store.has(k) ? 'x' : null
  },
  async put(k: string, v: string | ArrayBuffer, opts?: { metadata?: unknown }) {
    store.set(k, { v: typeof v === 'string' ? enc(v) : v, m: opts?.metadata ?? null })
  },
  async delete(k: string) {
    store.delete(k)
  },
  async list({ prefix }: { prefix: string }) {
    const keys = [...store.keys()]
      .filter((k) => k.startsWith(prefix))
      .map((k) => ({ name: k, metadata: store.get(k)!.m }))
    return { keys, list_complete: true, cursor: null }
  },
}
const env = { SITES, BASE_DOMAIN: BASE }

const fetchIt = (req: Request) => worker.fetch(req, env as never)
const get = (url: string, init?: RequestInit) => fetchIt(new Request(url, init))

const sha256hex = async (s: string) =>
  [...new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(s)))]
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('')

put('abc:/index.html', '<h1>root</h1>', { etag: '"e1"' })
put('abc:/style.css', 'body{}')
put('abc:/docs/index.html', '<h1>docs</h1>')
put('abc:/404.html', '<h1>custom nf</h1>')
put('bare:/index.html', 'bare')
record('abc')
record('bare')

// ---------- routing ----------

test('routes hosts and paths to the right object', async () => {
  const cases: [string, string, number, string][] = [
    ['GET', at('abc'), 200, 'text/html'],
    ['GET', at('abc', '/index.html'), 200, 'text/html'],
    ['GET', at('abc', '/style.css'), 200, 'text/css'],
    ['GET', at('abc', '/docs'), 200, 'text/html'],
    ['GET', at('abc', '/docs/'), 200, 'text/html'],
    ['GET', at('abc', '/nope.html'), 404, 'text/html'],
    ['GET', at('nosuch'), 404, 'text/html'],
    ['GET', `https://${BASE}/`, 404, 'text/html'],
    ['GET', at('a.b'), 404, 'text/html'],
    ['GET', 'https://abc.evil.com/', 404, 'text/html'],
    ['POST', at('abc'), 405, 'text/html'],
    ['HEAD', at('abc'), 200, 'text/html'],
    // the URL parser collapses %2e%2e before the worker runs, so this is a plain miss
    ['GET', at('abc', '/%2e%2e/secret'), 404, 'text/html'],
    ['GET', at('abc', '/%e0%a4%a'), 400, 'text/html'],
  ]
  for (const [method, url, status, type] of cases) {
    const res = await get(url, { method })
    expect(`${method} ${url} -> ${res.status}`).toBe(`${method} ${url} -> ${status}`)
    expect(res.headers.get('content-type')).toContain(type)
  }
})

test('falls back to the site 404 page', async () => {
  expect(await (await get(at('abc', '/nope.html'))).text()).toContain('custom nf')
})

test('honours a matching etag', async () => {
  const res = await get(at('abc'), { headers: { 'if-none-match': '"e1"' } })
  expect(res.status).toBe(304)
})

test('keeps pages out of search indexes', async () => {
  expect((await get(at('abc'))).headers.get('x-robots-tag')).toContain('noindex')
})

// ---------- basic auth ----------

const SALT = 'a1b2c3d4e5f60718a1b2c3d4e5f60718'
const auth = { salt: SALT, hash: await sha256hex(`${SALT}sendup:hunter2`) }
const basic = (u: string, p: string) => ({
  authorization: 'Basic ' + Buffer.from(`${u}:${p}`).toString('base64'),
})

put('sec:/index.html', '<h1>secret</h1>')
put('sec:/deep/page.html', 'deep')
record('sec', { auth })

test('challenges for a password and rejects wrong ones', async () => {
  const chal = await get(at('sec'))
  expect(chal.status).toBe(401)
  expect(chal.headers.get('www-authenticate')).toStartWith('Basic realm=')

  expect((await get(at('sec'), { headers: basic('sendup', 'nope') })).status).toBe(401)
  expect((await get(at('sec'), { headers: basic('admin', 'hunter2') })).status).toBe(401)
  expect((await get(at('sec'), { headers: { authorization: 'Basic !!!' } })).status).toBe(401)
  expect((await get(at('sec'), { headers: { authorization: 'Bearer x' } })).status).toBe(401)
})

test('serves the page once the password is right', async () => {
  const res = await get(at('sec'), { headers: basic('sendup', 'hunter2') })
  expect(res.status).toBe(200)
  expect(await res.text()).toContain('secret')
  expect(res.headers.get('cache-control')).toBe('private, no-store')
  expect(res.headers.get('vary')).toBe('Authorization, Cookie')
})

test('gates a miss too, so paths cannot be probed', async () => {
  expect((await get(at('sec', '/whatever'))).status).toBe(401)
  const authed = await get(at('sec', '/whatever'), { headers: basic('sendup', 'hunter2') })
  expect(authed.status).toBe(404)
})

test('leaves public sites open', async () => {
  expect((await get(at('abc'))).status).toBe(200)
})

// ---------- signed links ----------

const SECRET = '00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff'
const mint = async (slug: string, exp: number) => {
  const key = await crypto.subtle.importKey(
    'raw',
    Uint8Array.from(SECRET.match(/../g)!.map((h) => parseInt(h, 16))),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  )
  const mac = new Uint8Array(
    await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(`${slug}:${exp}`))
  )
  return `${exp}.${[...mac.slice(0, 16)].map((b) => b.toString(16).padStart(2, '0')).join('')}`
}

put('lnk:/index.html', '<h1>link only</h1>')
record('lnk', { link: SECRET })
put('both:/index.html', '<h1>both</h1>')
record('both', { link: SECRET, auth })

const forever = await mint('lnk', 0)
const expired = await mint('lnk', Math.floor(Date.now() / 1000) - 60)
const future = await mint('lnk', Math.floor(Date.now() / 1000) + 3600)
const cookie = (t: string) => ({ cookie: `sendup_link=${t}` })

test('refuses every kind of bad token', async () => {
  expect((await get(at('lnk'))).status).toBe(403)
  expect((await get(at('lnk', '/?k=nonsense'))).status).toBe(403)
  expect((await get(at('lnk', `/?k=0.${'0'.repeat(32)}`))).status).toBe(403)
  expect((await get(at('lnk', `/?k=${expired}`))).status).toBe(403)
  // signed for a different site
  expect((await get(at('lnk', `/?k=${await mint('sec', 0)}`))).status).toBe(403)
})

test('trades a valid token for a cookie and a clean URL', async () => {
  const res = await get(at('lnk', `/?k=${forever}`))
  expect(res.status).toBe(302)
  const setCookie = res.headers.get('set-cookie') ?? ''
  expect(setCookie).toContain('sendup_link=')
  expect(setCookie).toContain('HttpOnly')
  expect(setCookie).toContain('Secure')
  expect(res.headers.get('location')).not.toContain('k=')
})

test('accepts the cookie on later requests', async () => {
  const res = await get(at('lnk'), { headers: cookie(forever) })
  expect(res.status).toBe(200)
  expect(await res.text()).toContain('link only')
  expect(res.headers.get('vary')).toBe('Authorization, Cookie')

  expect((await get(at('lnk'), { headers: cookie(expired) })).status).toBe(403)
  expect((await get(at('lnk', `/?k=${future}`))).status).toBe(302)
  expect((await get(at('lnk', '/nope'))).status).toBe(403)
})

test('either credential opens a site carrying both', async () => {
  expect((await get(at('both'), { headers: cookie(await mint('both', 0)) })).status).toBe(200)
  expect((await get(at('both'), { headers: basic('sendup', 'hunter2') })).status).toBe(200)
  // with neither, the password prompt is the useful answer
  expect((await get(at('both'))).status).toBe(401)
})

// ---------- dashboard ----------

const ADMIN_SALT = 'ffeeddccbbaa99887766554433221100'
store.set('__admin', {
  v: enc(''),
  m: { salt: ADMIN_SALT, hash: await sha256hex(`${ADMIN_SALT}admin:letmein`) },
})

const DASH = `https://dash.${BASE}`
const admin = basic('admin', 'letmein')
const post = (path: string, body: unknown = {}, extra: HeadersInit = {}) =>
  fetchIt(
    new Request(DASH + path, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-sendup': '1', ...admin, ...extra },
      body: JSON.stringify(body),
    })
  )

test('dashboard demands its own password', async () => {
  expect((await get(DASH + '/')).status).toBe(401)
  expect((await get(DASH + '/', { headers: basic('admin', 'nope') })).status).toBe(401)
})

test('dashboard serves the built UI with the domain injected', async () => {
  const res = await get(DASH + '/', { headers: admin })
  expect(res.status).toBe(200)
  const body = await res.text()
  expect(body).toContain('<title>sendup</title>')
  expect(body).not.toContain('__BASE__')
  expect(body).toContain(`window.__SENDUP_BASE__="${BASE}"`)
  // dark unless the visitor has chosen otherwise, decided before first paint
  expect(body).toContain('data-mode="dark"')
  expect(body).toContain("localStorage.getItem('sendup:mode')")
})

test('mutating calls need same-origin proof, not just a password', async () => {
  // browsers replay Basic auth cross-site, so the custom header is the real guard
  const noHeader = await fetchIt(
    new Request(DASH + '/api/list', { method: 'POST', headers: { ...admin }, body: '{}' })
  )
  expect(noHeader.status).toBe(400)
  expect((await post('/api/list', {}, { 'sec-fetch-site': 'cross-site' })).status).toBe(403)

  const noAuth = await fetchIt(
    new Request(DASH + '/api/list', { method: 'POST', headers: { 'x-sendup': '1' }, body: '{}' })
  )
  expect(noAuth.status).toBe(401)
  expect((await get(DASH + '/api/list', { headers: admin })).status).toBe(404)
  expect((await post('/api/delete', { slug: 'Not A Slug' })).status).toBe(400)
})

test('lists every site with its access mode', async () => {
  const res = await post('/api/list')
  expect(res.status).toBe(200)
  const d = (await res.json()) as { sites: { slug: string; access: string }[]; totals: { sites: number } }
  const mode = (slug: string) => d.sites.find((s) => s.slug === slug)?.access
  expect(mode('sec')).toBe('auth')
  expect(mode('lnk')).toBe('link')
  expect(mode('abc')).toBe('open')
  expect(mode('both')).toBe('both')
  expect(d.totals.sites).toBe(d.sites.length)
})

test('switching access changes what the site serves', async () => {
  expect((await post('/api/access', { slug: 'abc', mode: 'link' })).status).toBe(200)
  expect((await get(at('abc'))).status).toBe(403)

  const issued = await post('/api/link', { slug: 'abc' })
  expect(issued.status).toBe(200)
  const { url } = (await issued.json()) as { url: string }
  expect((await get(url)).status).toBe(302)

  expect((await post('/api/rotate', { slug: 'abc' })).status).toBe(200)
  expect((await get(url)).status).toBe(403)

  const pw = await post('/api/access', { slug: 'abc', mode: 'auth', user: 'me', pass: 'pw123' })
  expect(((await pw.json()) as { password: string }).password).toBe('me / pw123')
  expect((await get(at('abc'))).status).toBe(401)
  expect((await get(at('abc'), { headers: basic('me', 'pw123') })).status).toBe(200)

  expect((await post('/api/access', { slug: 'abc', mode: 'open' })).status).toBe(200)
  expect((await get(at('abc'))).status).toBe(200)

  expect((await post('/api/access', { slug: 'abc', mode: 'bogus' })).status).toBe(400)
  expect((await post('/api/link', { slug: 'abc' })).status).toBe(400)
})

// ---------- rename ----------

put('ren:/index.html', '<h1>renamed</h1>', { etag: '"r1"' })
put('ren:/deep/a.css', 'body{}')
record('ren', { created: '2026-08-01T00:00:00Z', files: 2, link: SECRET })

test('rejects a rename it cannot honour', async () => {
  expect((await post('/api/rename', { slug: 'ren', to: 'Not Valid' })).status).toBe(400)
  expect((await post('/api/rename', { slug: 'ren', to: 'dash' })).status).toBe(400)
  expect((await post('/api/rename', { slug: 'ren', to: 'ren' })).status).toBe(400)
  expect((await post('/api/rename', { slug: 'ren', to: 'sec' })).status).toBe(409)
  expect((await post('/api/rename', { slug: 'nosuchsite', to: 'whatever' })).status).toBe(404)
})

test('rename moves every file and kills the old name', async () => {
  const res = await post('/api/rename', { slug: 'ren', to: 'report' })
  expect(res.status).toBe(200)
  expect(await res.json()).toEqual({
    ok: true,
    slug: 'report',
    files: 2,
    linksInvalidated: true,
  })

  expect((await get(at('report', `/?k=${await mint('report', 0)}`))).status).toBe(302)
  expect((await get(at('ren'))).status).toBe(404)

  expect(store.has('ren:/index.html')).toBe(false)
  expect(store.has('report:/index.html')).toBe(true)
  expect(store.has('report:/deep/a.css')).toBe(true)
  expect((store.get('report:/index.html')!.m as { etag: string }).etag).toBe('"r1"')

  // a token signed for the old slug must not open the new one
  expect((await get(at('report', `/?k=${await mint('ren', 0)}`))).status).toBe(403)
})

// ---------- delete ----------

test('delete takes the files with it', async () => {
  expect((await post('/api/delete', { slug: 'bare' })).status).toBe(200)
  expect((await get(at('bare'))).status).toBe(404)
  expect(store.has('bare:/index.html')).toBe(false)
})
