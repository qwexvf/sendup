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
const auth = { salt: SALT, hash: await sha256hex(SALT + 'devdox:hunter2') }

put('sec:/index.html', '<h1>secret</h1>', { auth })
put('sec:/deep/page.html', 'deep', { auth })
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

await check('auth: wrong password', new Request('https://sec.example.com/', { headers: basic('devdox', 'nope') }), 401)
await check('auth: wrong user', new Request('https://sec.example.com/', { headers: basic('admin', 'hunter2') }), 401)
await check('auth: malformed header', new Request('https://sec.example.com/', { headers: { authorization: 'Basic !!!' } }), 401)
await check('auth: bearer instead of basic', new Request('https://sec.example.com/', { headers: { authorization: 'Bearer x' } }), 401)
const good = await check('auth: correct credentials', new Request('https://sec.example.com/', { headers: basic('devdox', 'hunter2') }), 200)
console.log((await good.text()).includes('secret') ? 'ok   auth: body served once authed' : 'FAIL auth: body missing')
console.log(
  good.headers.get('cache-control') === 'private, no-store' && good.headers.get('vary') === 'Authorization'
    ? 'ok   auth: response marked uncacheable'
    : `FAIL auth: cache headers ${good.headers.get('cache-control')} / ${good.headers.get('vary')}`
)

// a miss on a protected site must not reveal whether the path exists
await check('auth: miss is gated too', new Request('https://sec.example.com/whatever'), 401)
await check('auth: miss with credentials 404s', new Request('https://sec.example.com/whatever', { headers: basic('devdox', 'hunter2') }), 404)
// public sites keep working untouched
await check('public site still open', new Request('https://abc.example.com/'), 200)

process.exit(fail ? 1 : 0)
