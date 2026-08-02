import worker from '../src/worker.js'

const store = new Map()
const put = (k, v, m) => store.set(k, { v: new TextEncoder().encode(v).buffer, m })
put('abc:/index.html', '<h1>root</h1>', { etag: '"e1"' })
put('abc:/style.css', 'body{}', null)
put('abc:/docs/index.html', '<h1>docs</h1>', null)
put('abc:/404.html', '<h1>custom nf</h1>', null)
put('bare:/index.html', 'bare', null)

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

process.exit(fail ? 1 : 0)
