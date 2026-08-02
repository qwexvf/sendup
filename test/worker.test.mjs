import worker from '../src/worker.js'

const store = new Map()
const put = (k, v, m) => store.set(k, { v: new TextEncoder().encode(v).buffer, m })
put('abc:/index.html', '<h1>root</h1>', { etag: '"e1"' })
put('abc:/style.css', 'body{}', null)
put('abc:/docs/index.html', '<h1>docs</h1>', null)
put('abc:/404.html', '<h1>custom nf</h1>', null)
put('bare:/index.html', 'bare', null)
// site records; the worker consults these on a miss
store.set('__index:abc', { v: new TextEncoder().encode('').buffer, m: {} })
store.set('__index:bare', { v: new TextEncoder().encode('').buffer, m: {} })

const SITES = {
  async getWithMetadata(k) {
    const e = store.get(k)
    return e ? { value: e.v, metadata: e.m } : { value: null, metadata: null }
  },
  async get(k) {
    return store.has(k) ? 'x' : null
  },
  async put(k, v, opts) {
    store.set(k, { v: new TextEncoder().encode(v).buffer, m: opts?.metadata ?? null })
  },
  async delete(k) {
    store.delete(k)
  },
  async list({ prefix }) {
    const keys = [...store.keys()]
      .filter((k) => k.startsWith(prefix))
      .map((k) => ({ name: k, metadata: store.get(k).m }))
    return { keys, list_complete: true, cursor: null }
  },
}
const env = { SITES, BASE_DOMAIN: 'example.com' }

const cases = [
  ['GET', 'https://abc.example.com/', 200, 'text/html'],
  ['GET', 'https://abc.example.com/index.html', 200, 'text/html'],
  ['GET', 'https://abc.example.com/style.css', 200, 'text/css'],
  ['GET', 'https://abc.example.com/docs', 200, 'text/html'],
  ['GET', 'https://abc.example.com/docs/', 200, 'text/html'],
  ['GET', 'https://abc.example.com/nope.html', 404, 'text/html'],
  ['GET', 'https://nosuch.example.com/', 404, 'text/html'],
  ['GET', 'https://example.com/', 404, 'text/html'],
  ['GET', 'https://a.b.example.com/', 404, 'text/html'],
  ['GET', 'https://abc.evil.com/', 404, 'text/html'],
  ['POST', 'https://abc.example.com/', 405, 'text/html'],
  ['HEAD', 'https://abc.example.com/', 200, 'text/html'],
  // the URL parser collapses %2e%2e before the worker runs, so this is a plain miss
  ['GET', 'https://abc.example.com/%2e%2e/secret', 404, 'text/html'],
  ['GET', 'https://abc.example.com/%e0%a4%a', 400, 'text/html'],
]

let fail = 0
for (const [method, url, want, ct] of cases) {
  const res = await worker.fetch(new Request(url, { method }), env)
  const ok = res.status === want && res.headers.get('content-type').includes(ct)
  if (!ok) fail++
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${method} ${url} -> ${res.status} ${res.headers.get('content-type')}`)
}

// custom 404 body
const nf = await worker.fetch(new Request('https://abc.example.com/nope.html'), env)
console.log((await nf.text()).includes('custom nf') ? 'ok   custom 404 served' : 'FAIL custom 404')

// etag / 304
const e = await worker.fetch(new Request('https://abc.example.com/', { headers: { 'if-none-match': '"e1"' } }), env)
console.log(e.status === 304 ? 'ok   304 on matching etag' : `FAIL etag -> ${e.status}`)

// noindex header
const h = await worker.fetch(new Request('https://abc.example.com/'), env)
console.log(h.headers.get('x-robots-tag')?.includes('noindex') ? 'ok   noindex header' : 'FAIL noindex')

// ---- basic auth ----

const SALT = 'a1b2c3d4e5f60718a1b2c3d4e5f60718'
const sha256hex = async (s) =>
  [...new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(s)))]
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('')
const auth = { salt: SALT, hash: await sha256hex(SALT + 'sendup:hunter2') }

put('sec:/index.html', '<h1>secret</h1>', null)
put('sec:/deep/page.html', 'deep', null)
store.set('__index:sec', { v: new TextEncoder().encode('').buffer, m: { auth } })

const basic = (u, p) => ({ authorization: 'Basic ' + Buffer.from(`${u}:${p}`).toString('base64') })
const check = async (label, req, want) => {
  const res = await worker.fetch(req, env)
  const ok = res.status === want
  if (!ok) fail++
  console.log(`${ok ? 'ok  ' : 'FAIL'} ${label} -> ${res.status} (want ${want})`)
  return res
}

const chal = await check('auth: no credentials', new Request('https://sec.example.com/'), 401)
console.log(
  chal.headers.get('www-authenticate')?.startsWith('Basic realm=')
    ? 'ok   auth: WWW-Authenticate challenge sent'
    : 'FAIL auth: missing WWW-Authenticate'
)

await check('auth: wrong password', new Request('https://sec.example.com/', { headers: basic('sendup', 'nope') }), 401)
await check('auth: wrong user', new Request('https://sec.example.com/', { headers: basic('admin', 'hunter2') }), 401)
await check('auth: malformed header', new Request('https://sec.example.com/', { headers: { authorization: 'Basic !!!' } }), 401)
await check('auth: bearer instead of basic', new Request('https://sec.example.com/', { headers: { authorization: 'Bearer x' } }), 401)
const good = await check('auth: correct credentials', new Request('https://sec.example.com/', { headers: basic('sendup', 'hunter2') }), 200)
console.log((await good.text()).includes('secret') ? 'ok   auth: body served once authed' : 'FAIL auth: body missing')
console.log(
  good.headers.get('cache-control') === 'private, no-store' &&
    good.headers.get('vary') === 'Authorization, Cookie'
    ? 'ok   auth: response marked uncacheable'
    : `FAIL auth: cache headers ${good.headers.get('cache-control')} / ${good.headers.get('vary')}`
)

// a miss on a protected site must not reveal whether the path exists
await check('auth: miss is gated too', new Request('https://sec.example.com/whatever'), 401)
await check('auth: miss with credentials 404s', new Request('https://sec.example.com/whatever', { headers: basic('sendup', 'hunter2') }), 404)
// public sites keep working untouched
await check('public site still open', new Request('https://abc.example.com/'), 200)

// ---- signed links ----

const SECRET = '00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff'
const mint = async (slug, exp) => {
  const key = await crypto.subtle.importKey(
    'raw',
    Uint8Array.from(SECRET.match(/../g).map((h) => parseInt(h, 16))),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  )
  const mac = new Uint8Array(await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(`${slug}:${exp}`)))
  return `${exp}.${[...mac.slice(0, 16)].map((b) => b.toString(16).padStart(2, '0')).join('')}`
}

put('lnk:/index.html', '<h1>link only</h1>', null)
store.set('__index:lnk', { v: new TextEncoder().encode('').buffer, m: { link: SECRET } })
// a site carrying both credentials — either should open it
put('both:/index.html', '<h1>both</h1>', null)
store.set('__index:both', { v: new TextEncoder().encode('').buffer, m: { link: SECRET, auth } })

const forever = await mint('lnk', 0)
const expired = await mint('lnk', Math.floor(Date.now() / 1000) - 60)
const future = await mint('lnk', Math.floor(Date.now() / 1000) + 3600)
const otherSlug = await mint('sec', 0) // signed for a different site

await check('link: no token', new Request('https://lnk.example.com/'), 403)
await check('link: garbage token', new Request('https://lnk.example.com/?k=nonsense'), 403)
await check('link: tampered signature', new Request(`https://lnk.example.com/?k=0.${'0'.repeat(32)}`), 403)
await check('link: expired token', new Request(`https://lnk.example.com/?k=${expired}`), 403)
await check('link: token signed for another site', new Request(`https://lnk.example.com/?k=${otherSlug}`), 403)
await check('link: no basic-auth prompt on link-only site', new Request('https://lnk.example.com/'), 403)

const granted = await check('link: valid token redirects', new Request(`https://lnk.example.com/?k=${forever}`), 302)
const setCookie = granted.headers.get('set-cookie') || ''
console.log(
  setCookie.includes('sendup_link=') && setCookie.includes('HttpOnly') && setCookie.includes('Secure')
    ? 'ok   link: cookie is HttpOnly + Secure'
    : `FAIL link: cookie ${setCookie}`
)
console.log(
  !granted.headers.get('location').includes('k=')
    ? 'ok   link: token stripped from redirect target'
    : `FAIL link: token left in ${granted.headers.get('location')}`
)

const cookie = (t) => ({ cookie: `sendup_link=${t}` })
const viaCookie = await check('link: cookie grants access', new Request('https://lnk.example.com/', { headers: cookie(forever) }), 200)
console.log((await viaCookie.text()).includes('link only') ? 'ok   link: body served' : 'FAIL link: body missing')
console.log(
  viaCookie.headers.get('vary') === 'Authorization, Cookie'
    ? 'ok   link: varies on Cookie'
    : `FAIL link: vary ${viaCookie.headers.get('vary')}`
)
await check('link: expired cookie rejected', new Request('https://lnk.example.com/', { headers: cookie(expired) }), 403)
await check('link: dated token still valid', new Request(`https://lnk.example.com/?k=${future}`), 302)
await check('link: miss is gated too', new Request('https://lnk.example.com/nope'), 403)

// either credential opens a site carrying both
await check('both: link alone works', new Request('https://both.example.com/', { headers: cookie(await mint('both', 0)) }), 200)
await check('both: password alone works', new Request('https://both.example.com/', { headers: basic('sendup', 'hunter2') }), 200)
await check('both: neither is refused with a prompt', new Request('https://both.example.com/'), 401)

// ---- dashboard ----

const ADMIN_SALT = 'ffeeddccbbaa99887766554433221100'
store.set('__admin', {
  v: new TextEncoder().encode('').buffer,
  m: { salt: ADMIN_SALT, hash: await sha256hex(ADMIN_SALT + 'admin:letmein') },
})

const DASH = 'https://dash.example.com'
const admin = basic('admin', 'letmein')
const post = (path, body, extra = {}) =>
  new Request(DASH + path, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-sendup': '1', ...admin, ...extra },
    body: JSON.stringify(body || {}),
  })

await check('dash: no credentials', new Request(DASH + '/'), 401)
await check('dash: wrong password', new Request(DASH + '/', { headers: basic('admin', 'nope') }), 401)
const ui = await check('dash: UI served when authed', new Request(DASH + '/', { headers: admin }), 200)
const uiBody = await ui.text()
console.log(uiBody.includes('<title>sendup</title>') ? 'ok   dash: UI is html' : 'FAIL dash: UI body')
console.log(
  !uiBody.includes('__BASE__') && uiBody.includes('window.__SENDUP_BASE__="example.com"')
    ? 'ok   dash: base domain injected'
    : 'FAIL dash: base domain not substituted'
)

// CSRF: browsers replay Basic auth cross-site, so the custom header is the real guard
await check(
  'dash: rejects call without x-sendup header',
  new Request(DASH + '/api/list', { method: 'POST', headers: { ...admin }, body: '{}' }),
  400
)
await check(
  'dash: rejects cross-origin call',
  post('/api/list', {}, { 'sec-fetch-site': 'cross-site' }),
  403
)
await check('dash: api needs auth too', new Request(DASH + '/api/list', { method: 'POST', headers: { 'x-sendup': '1' }, body: '{}' }), 401)
await check('dash: GET on api is 404', new Request(DASH + '/api/list', { headers: admin }), 404)
await check('dash: bad slug rejected', post('/api/delete', { slug: 'Not A Slug' }), 400)

const listed = await check('dash: list', post('/api/list'), 200)
const data = await listed.json()
console.log(
  data.sites.some((s) => s.slug === 'sec' && s.access === 'auth') &&
    data.sites.some((s) => s.slug === 'lnk' && s.access === 'link') &&
    data.sites.some((s) => s.slug === 'abc' && s.access === 'open') &&
    data.sites.some((s) => s.slug === 'both' && s.access === 'both')
    ? 'ok   dash: access mode reported per site'
    : `FAIL dash: modes ${JSON.stringify(data.sites.map((s) => [s.slug, s.access]))}`
)
console.log(data.totals.sites === data.sites.length ? 'ok   dash: totals' : 'FAIL dash: totals')

// toggling access must actually change what the site serves
await check('dash: set abc to link-only', post('/api/access', { slug: 'abc', mode: 'link' }), 200)
await check('dash: abc now refuses anonymous', new Request('https://abc.example.com/'), 403)
const issued = await check('dash: mint a link for abc', post('/api/link', { slug: 'abc' }), 200)
const issuedURL = (await issued.json()).url
await check('dash: minted link opens abc', new Request(issuedURL), 302)

await check('dash: rotate revokes it', post('/api/rotate', { slug: 'abc' }), 200)
await check('dash: old link now dead', new Request(issuedURL), 403)

const pw = await check('dash: switch abc to password', post('/api/access', { slug: 'abc', mode: 'auth', user: 'me', pass: 'pw123' }), 200)
console.log((await pw.json()).password === 'me / pw123' ? 'ok   dash: password echoed once' : 'FAIL dash: password echo')
await check('dash: abc prompts for password', new Request('https://abc.example.com/'), 401)
await check('dash: abc opens with password', new Request('https://abc.example.com/', { headers: basic('me', 'pw123') }), 200)

await check('dash: back to open', post('/api/access', { slug: 'abc', mode: 'open' }), 200)
await check('dash: abc public again', new Request('https://abc.example.com/'), 200)
await check('dash: bad mode rejected', post('/api/access', { slug: 'abc', mode: 'bogus' }), 400)
await check('dash: link on a non-link site', post('/api/link', { slug: 'abc' }), 400)

// ---- rename ----

put('ren:/index.html', '<h1>renamed</h1>', { etag: '"r1"' })
put('ren:/deep/a.css', 'body{}', null)
store.set('__index:ren', { v: new TextEncoder().encode('').buffer, m: { created: '2026-08-01T00:00:00Z', files: 2, link: SECRET } })

await check('rename: bad target', post('/api/rename', { slug: 'ren', to: 'Not Valid' }), 400)
await check('rename: reserved target', post('/api/rename', { slug: 'ren', to: 'dash' }), 400)
await check('rename: same name', post('/api/rename', { slug: 'ren', to: 'ren' }), 400)
await check('rename: target taken', post('/api/rename', { slug: 'ren', to: 'sec' }), 409)
await check('rename: unknown source', post('/api/rename', { slug: 'nosuchsite', to: 'whatever' }), 404)

const renamed = await check('rename: succeeds', post('/api/rename', { slug: 'ren', to: 'report' }), 200)
const rBody = await renamed.json()
console.log(
  rBody.slug === 'report' && rBody.files === 2 && rBody.linksInvalidated === true
    ? 'ok   rename: reports new slug, file count and dead links'
    : `FAIL rename: ${JSON.stringify(rBody)}`
)

await check('rename: serves at the new name', new Request(`https://report.example.com/?k=${await mint('report', 0)}`), 302)
await check('rename: old name is gone', new Request('https://ren.example.com/'), 404)
console.log(
  !store.has('ren:/index.html') && store.has('report:/index.html') && store.has('report:/deep/a.css')
    ? 'ok   rename: every file moved, none left behind'
    : 'FAIL rename: files not moved cleanly'
)
console.log(
  store.get('report:/index.html').m?.etag === '"r1"'
    ? 'ok   rename: per-file metadata survives'
    : 'FAIL rename: metadata lost'
)
// a token signed for the old slug must not open the new one
await check('rename: old signed link is dead', new Request(`https://report.example.com/?k=${await mint('ren', 0)}`), 403)

// delete must take the files with it
await check('dash: delete bare', post('/api/delete', { slug: 'bare' }), 200)
await check('dash: deleted site is gone', new Request('https://bare.example.com/'), 404)
console.log(!store.has('bare:/index.html') ? 'ok   dash: files removed on delete' : 'FAIL dash: files left behind')

process.exit(fail ? 1 : 0)
