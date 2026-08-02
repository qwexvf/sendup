// Serves static sites out of KV, keyed by subdomain slug, plus an admin dashboard on
// dash.<domain>.
//
// KV layout:
//   "<slug>:<path>"   file bytes, metadata { etag }
//   "__index:<slug>"  site record, metadata { created, files, bytes, src, path, title,
//                                             expires?, auth?, link? }
//   "__admin"         dashboard credentials, metadata { salt, hash }
//
// Access config lives only on the site record, so changing it is one write rather than
// one per file. Costs one extra KV read per request, issued in parallel with the file.

const TYPES = {
  html: 'text/html; charset=utf-8',
  htm: 'text/html; charset=utf-8',
  css: 'text/css; charset=utf-8',
  js: 'text/javascript; charset=utf-8',
  mjs: 'text/javascript; charset=utf-8',
  json: 'application/json; charset=utf-8',
  svg: 'image/svg+xml',
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  gif: 'image/gif',
  webp: 'image/webp',
  avif: 'image/avif',
  ico: 'image/x-icon',
  woff: 'font/woff',
  woff2: 'font/woff2',
  ttf: 'font/ttf',
  otf: 'font/otf',
  txt: 'text/plain; charset=utf-8',
  md: 'text/markdown; charset=utf-8',
  xml: 'application/xml; charset=utf-8',
  pdf: 'application/pdf',
  wasm: 'application/wasm',
  map: 'application/json; charset=utf-8',
}

const contentType = (path, meta) =>
  (meta && meta.ct) || TYPES[path.slice(path.lastIndexOf('.') + 1).toLowerCase()] || 'application/octet-stream'

function headers(path, meta, gated) {
  const h = new Headers()
  h.set('content-type', contentType(path, meta))
  // unlisted, not secret — keep it out of search indexes
  h.set('x-robots-tag', 'noindex, nofollow, noarchive')
  h.set('x-content-type-options', 'nosniff')
  h.set('referrer-policy', 'no-referrer')
  if (gated) {
    // never let a shared cache hold a credentialed response
    h.set('cache-control', 'private, no-store')
    h.set('vary', 'Authorization, Cookie')
  } else {
    h.set('cache-control', 'public, max-age=60, must-revalidate')
  }
  if (meta && meta.etag) h.set('etag', meta.etag)
  return h
}

// ---------- crypto helpers ----------

function hexToBytes(hex) {
  if (typeof hex !== 'string' || hex.length % 2 !== 0) return null
  const out = new Uint8Array(hex.length / 2)
  for (let i = 0; i < out.length; i++) {
    const b = Number.parseInt(hex.substr(i * 2, 2), 16)
    if (Number.isNaN(b)) return null
    out[i] = b
  }
  return out
}

const toHex = (bytes) => [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('')

function constantTimeEqual(a, b) {
  if (a.length !== b.length) return false
  let diff = 0
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i]
  return diff === 0
}

const sha256 = async (s) => new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(s)))

function randomHex(bytes) {
  const b = new Uint8Array(bytes)
  crypto.getRandomValues(b)
  return toHex(b)
}

// creds = { salt, hash } where hash is sha256(salt + "user:pass")
async function checkCreds(header, creds) {
  if (!creds || typeof creds.salt !== 'string') return false
  const expected = hexToBytes(creds.hash)
  if (!expected) return false
  if (typeof header !== 'string' || !header.startsWith('Basic ')) return false

  let decoded
  try {
    // atob gives latin1 bytes; recover the original utf-8 so non-ascii passwords work
    const raw = atob(header.slice(6).trim())
    decoded = new TextDecoder().decode(Uint8Array.from(raw, (c) => c.charCodeAt(0)))
  } catch {
    return false
  }
  return constantTimeEqual(await sha256(creds.salt + decoded), expected)
}

async function hmac16(secretHex, message) {
  const key = await crypto.subtle.importKey(
    'raw',
    hexToBytes(secretHex),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign']
  )
  const mac = new Uint8Array(await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(message)))
  return mac.slice(0, 16)
}

// token = "<exp>.<sig>", exp is unix seconds (0 = never). The expiry is inside the
// signed message, so a recipient cannot extend their own link.
async function validToken(token, slug, secretHex) {
  if (typeof token !== 'string') return false
  const dot = token.indexOf('.')
  if (dot < 1) return false

  const exp = Number(token.slice(0, dot))
  if (!Number.isSafeInteger(exp) || exp < 0) return false
  if (exp !== 0 && exp * 1000 < Date.now()) return false

  const sig = hexToBytes(token.slice(dot + 1))
  if (!sig || sig.length !== 16 || !hexToBytes(secretHex)) return false
  return constantTimeEqual(sig, await hmac16(secretHex, `${slug}:${exp}`))
}

const mintToken = async (secretHex, slug, exp) => `${exp}.${toHex(await hmac16(secretHex, `${slug}:${exp}`))}`

// ---------- gating ----------

const COOKIE = 'sendup_link'

function cookieValue(request, name) {
  const jar = request.headers.get('cookie')
  if (!jar) return null
  for (const part of jar.split(';')) {
    const eq = part.indexOf('=')
    if (eq > 0 && part.slice(0, eq).trim() === name) return part.slice(eq + 1).trim()
  }
  return null
}

// Hand over the cookie and bounce to the clean URL, so the token stops riding along in
// the address bar, bookmarks and any link the visitor copies onward.
function grantCookie(url, token) {
  const clean = new URL(url)
  clean.searchParams.delete('k')
  const exp = Number(token.slice(0, token.indexOf('.')))
  const maxAge = exp === 0 ? 60 * 60 * 24 * 30 : Math.max(60, exp - Math.floor(Date.now() / 1000))
  return new Response(null, {
    status: 302,
    headers: {
      location: clean.pathname + clean.search + clean.hash,
      'set-cookie': `${COOKIE}=${token}; Path=/; Max-Age=${maxAge}; HttpOnly; Secure; SameSite=Lax`,
      'cache-control': 'private, no-store',
      'x-robots-tag': 'noindex',
    },
  })
}

const unauthorized = (realm) =>
  new Response('401 — authentication required', {
    status: 401,
    headers: {
      'www-authenticate': `Basic realm="${realm}", charset="UTF-8"`,
      'content-type': 'text/plain; charset=utf-8',
      'cache-control': 'private, no-store',
      'x-robots-tag': 'noindex',
    },
  })

const forbidden = () =>
  new Response('403 — this link is invalid or has expired', {
    status: 403,
    headers: {
      'content-type': 'text/plain; charset=utf-8',
      'cache-control': 'private, no-store',
      'x-robots-tag': 'noindex',
    },
  })

// A site may carry a signed-link secret, a basic-auth hash, both, or neither; holding
// either credential is enough. Returns null to allow, a Response to stop.
async function gate(request, url, slug, meta) {
  if (!meta || (!meta.link && !meta.auth)) return null

  if (meta.link) {
    const q = url.searchParams.get('k')
    if (q && (await validToken(q, slug, meta.link))) return grantCookie(url, q)
    const c = cookieValue(request, COOKIE)
    if (c && (await validToken(c, slug, meta.link))) return null
  }
  if (meta.auth && (await checkCreds(request.headers.get('authorization'), meta.auth))) return null

  // a password box is only meaningful if a password would actually work
  return meta.auth ? unauthorized('sendup') : forbidden()
}

const page = (code, msg) =>
  new Response(
    `<!doctype html><meta charset=utf-8><title>${code}</title>` +
      `<style>body{font:16px/1.6 system-ui,sans-serif;display:grid;place-items:center;height:100vh;margin:0;background:#0b0b0c;color:#e6e6e6}` +
      `div{text-align:center}h1{font-size:3rem;margin:0 0 .25rem;font-weight:600}p{opacity:.6;margin:0}</style>` +
      `<div><h1>${code}</h1><p>${msg}</p></div>`,
    { status: code, headers: { 'content-type': 'text/html; charset=utf-8', 'x-robots-tag': 'noindex' } }
  )

// ---------- dashboard ----------

const json = (body, status = 200) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'private, no-store' },
  })

async function listSites(env) {
  const sites = []
  let cursor
  do {
    const page = await env.SITES.list({ prefix: '__index:', cursor })
    for (const k of page.keys) {
      const m = k.metadata || {}
      sites.push({
        slug: k.name.slice('__index:'.length),
        title: m.title || '',
        src: m.src || '',
        created: m.created || '',
        expires: m.expires || '',
        files: m.files || 0,
        bytes: m.bytes || 0,
        access: m.auth && m.link ? 'both' : m.auth ? 'auth' : m.link ? 'link' : 'open',
      })
    }
    cursor = page.list_complete ? null : page.cursor
  } while (cursor)
  sites.sort((a, b) => (b.created || '').localeCompare(a.created || ''))
  return sites
}

async function siteMeta(env, slug) {
  const rec = await env.SITES.getWithMetadata(`__index:${slug}`, { type: 'text' })
  return rec.metadata
}

async function writeMeta(env, slug, meta) {
  await env.SITES.put(`__index:${slug}`, '', { metadata: meta })
}

async function deleteSite(env, slug) {
  let cursor
  let n = 0
  do {
    const page = await env.SITES.list({ prefix: `${slug}:`, cursor })
    for (const k of page.keys) {
      await env.SITES.delete(k.name)
      n++
    }
    cursor = page.list_complete ? null : page.cursor
  } while (cursor)
  await env.SITES.delete(`__index:${slug}`)
  return n
}

async function handleDashboard(request, url, env, base) {
  const admin = await env.SITES.getWithMetadata('__admin', { type: 'text' })
  if (!admin.metadata) {
    return new Response(
      'dashboard is not set up yet — run `sendup admin --password <PASSWORD>` locally',
      { status: 503, headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' } }
    )
  }
  if (!(await checkCreds(request.headers.get('authorization'), admin.metadata))) {
    return unauthorized('sendup dashboard')
  }

  if (request.method === 'GET' && url.pathname === '/') {
    return new Response(DASHBOARD_HTML.replace('__BASE__', base), {
      headers: {
        'content-type': 'text/html; charset=utf-8',
        'cache-control': 'private, no-store',
        'x-robots-tag': 'noindex',
        'content-security-policy': "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:",
      },
    })
  }

  if (request.method !== 'POST' || !url.pathname.startsWith('/api/')) {
    return json({ error: 'not found' }, 404)
  }

  // Basic auth is replayed by browsers on cross-site requests, so require proof the
  // call came from our own page: a custom header (blocked cross-origin without a CORS
  // preflight we never answer) and a same-origin Sec-Fetch-Site when present.
  if (request.headers.get('x-sendup') !== '1') return json({ error: 'bad request' }, 400)
  const site = request.headers.get('sec-fetch-site')
  if (site && site !== 'same-origin') return json({ error: 'cross-origin blocked' }, 403)

  let body = {}
  try {
    body = await request.json()
  } catch {
    return json({ error: 'bad json' }, 400)
  }

  const slug = typeof body.slug === 'string' ? body.slug : ''
  const needsSlug = url.pathname !== '/api/list'
  if (needsSlug && !/^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$/.test(slug)) {
    return json({ error: 'bad slug' }, 400)
  }

  switch (url.pathname) {
    case '/api/list': {
      const sites = await listSites(env)
      return json({
        sites,
        totals: {
          sites: sites.length,
          files: sites.reduce((a, s) => a + s.files, 0),
          bytes: sites.reduce((a, s) => a + s.bytes, 0),
        },
      })
    }

    case '/api/delete': {
      const files = await deleteSite(env, slug)
      return json({ ok: true, files })
    }

    case '/api/access': {
      const meta = await siteMeta(env, slug)
      if (!meta) return json({ error: 'no such site' }, 404)
      const mode = body.mode
      const out = { ...meta }
      let password = null

      if (mode === 'open') {
        delete out.auth
        delete out.link
      } else if (mode === 'link') {
        delete out.auth
        out.link = out.link || randomHex(32)
      } else if (mode === 'auth') {
        delete out.link
        const user = typeof body.user === 'string' && body.user ? body.user : 'sendup'
        password = typeof body.pass === 'string' && body.pass ? body.pass : randomHex(8)
        const salt = randomHex(16)
        out.auth = { salt, hash: toHex(await sha256(salt + user + ':' + password)) }
        password = `${user} / ${password}`
      } else {
        return json({ error: 'bad mode' }, 400)
      }

      await writeMeta(env, slug, out)
      return json({ ok: true, password })
    }

    case '/api/link': {
      const meta = await siteMeta(env, slug)
      if (!meta) return json({ error: 'no such site' }, 404)
      if (!meta.link) return json({ error: 'site does not use signed links' }, 400)
      const days = Number(body.days)
      const exp = Number.isFinite(days) && days > 0 ? Math.floor(Date.now() / 1000 + days * 86400) : 0
      return json({ ok: true, url: `https://${slug}.${env.BASE_DOMAIN}/?k=${await mintToken(meta.link, slug, exp)}` })
    }

    case '/api/rotate': {
      const meta = await siteMeta(env, slug)
      if (!meta) return json({ error: 'no such site' }, 404)
      if (!meta.link) return json({ error: 'site does not use signed links' }, 400)
      await writeMeta(env, slug, { ...meta, link: randomHex(32) })
      return json({ ok: true })
    }
  }
  return json({ error: 'not found' }, 404)
}

// ---------- entrypoint ----------

export default {
  async fetch(request, env) {
    const url = new URL(request.url)
    const host = url.hostname.toLowerCase()
    const base = (env.BASE_DOMAIN || '').toLowerCase()

    if (!base || !host.endsWith('.' + base)) return page(404, 'unknown host')
    const slug = host.slice(0, -(base.length + 1))
    if (!slug || slug.includes('.')) return page(404, 'unknown host')

    if (slug === 'dash') return handleDashboard(request, url, env, base)

    if (request.method !== 'GET' && request.method !== 'HEAD') {
      return page(405, 'method not allowed')
    }

    let path = url.pathname
    try {
      path = decodeURIComponent(path)
    } catch {
      return page(400, 'bad path')
    }
    if (!path.startsWith('/')) path = '/' + path
    if (path.includes('..')) return page(400, 'bad path')
    if (path.endsWith('/')) path += 'index.html'

    // access config lives on the site record; fetch it alongside the file
    const [index, first] = await Promise.all([
      env.SITES.getWithMetadata(`__index:${slug}`, { type: 'text' }),
      env.SITES.getWithMetadata(`${slug}:${path}`, { type: 'arrayBuffer' }),
    ])
    if (index.metadata === null) return page(404, 'no such site')

    const blocked = await gate(request, url, slug, index.metadata)
    if (blocked) return blocked
    const gated = Boolean(index.metadata.auth || index.metadata.link)

    let hit = first
    // extensionless request -> try it as a directory
    if (hit.value === null && !path.slice(path.lastIndexOf('/')).includes('.')) {
      hit = await env.SITES.getWithMetadata(`${slug}:${path}/index.html`, { type: 'arrayBuffer' })
      if (hit.value !== null) path += '/index.html'
    }

    if (hit.value === null) {
      const custom = await env.SITES.getWithMetadata(`${slug}:/404.html`, { type: 'arrayBuffer' })
      if (custom.value !== null) {
        return new Response(custom.value, { status: 404, headers: headers('/404.html', custom.metadata, gated) })
      }
      return page(404, 'no such page')
    }

    const h = headers(path, hit.metadata, gated)
    const etag = h.get('etag')
    if (etag && request.headers.get('if-none-match') === etag) {
      return new Response(null, { status: 304, headers: h })
    }

    return new Response(request.method === 'HEAD' ? null : hit.value, { status: 200, headers: h })
  },
}

const DASHBOARD_HTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>sendup</title>
<style>
  :root{
    --bg:#f6f7f8; --card:#fff; --ink:#12181c; --ink2:#5b6b74; --ink3:#8697a0;
    --rule:#e2e7ea; --accent:#0e6a67; --accent-soft:#e4f0ef;
    --warn:#8a6a16; --warn-soft:#f6f0de; --risk:#a0392a; --risk-soft:#f8e9e5;
  }
  @media (prefers-color-scheme:dark){:root{
    --bg:#0f1417; --card:#161e22; --ink:#e7edef; --ink2:#a2b0b7; --ink3:#71828a;
    --rule:#243036; --accent:#4fbdb6; --accent-soft:#123230;
    --warn:#d9b25c; --warn-soft:#2c2517; --risk:#e08a76; --risk-soft:#31201c;
  }}
  *{box-sizing:border-box}
  body{margin:0;background:var(--bg);color:var(--ink);
    font:15px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif}
  .wrap{max-width:60rem;margin:0 auto;padding:1.5rem 1rem 5rem}
  header{display:flex;flex-wrap:wrap;gap:.75rem 1.5rem;align-items:baseline;
    padding-bottom:1rem;border-bottom:1px solid var(--rule);margin-bottom:1.25rem}
  h1{font-size:1.15rem;margin:0;letter-spacing:-.01em}
  .totals{font-size:.8rem;color:var(--ink3);font-variant-numeric:tabular-nums;
    display:flex;gap:1rem;flex-wrap:wrap}
  .card{background:var(--card);border:1px solid var(--rule);border-radius:8px;
    padding:.9rem 1rem;margin-bottom:.6rem;display:grid;
    grid-template-columns:1fr;gap:.7rem}
  @media(min-width:44rem){.card{grid-template-columns:1fr auto;align-items:center}}
  .who{min-width:0;display:flex;flex-direction:column;gap:.2rem}
  .name{display:flex;align-items:center;gap:.5rem;flex-wrap:wrap}
  .name a{color:var(--ink);text-decoration:none;font-weight:600;
    overflow-wrap:anywhere}
  .name a:hover{color:var(--accent);text-decoration:underline}
  .title{font-size:.83rem;color:var(--ink2);overflow-wrap:anywhere}
  .facts{font-size:.75rem;color:var(--ink3);font-variant-numeric:tabular-nums;
    display:flex;gap:.85rem;flex-wrap:wrap}
  .pill{font-size:.68rem;letter-spacing:.06em;text-transform:uppercase;
    padding:.12rem .45rem;border-radius:3px;font-weight:600;white-space:nowrap}
  .p-open{background:var(--warn-soft);color:var(--warn)}
  .p-link{background:var(--accent-soft);color:var(--accent)}
  .p-auth{background:var(--accent-soft);color:var(--accent)}
  .p-both{background:var(--accent-soft);color:var(--accent)}
  .acts{display:flex;gap:.35rem;flex-wrap:wrap}
  button{font:inherit;font-size:.79rem;padding:.35rem .6rem;border-radius:5px;
    border:1px solid var(--rule);background:var(--card);color:var(--ink2);
    cursor:pointer;white-space:nowrap}
  button:hover{border-color:var(--accent);color:var(--accent)}
  button.danger:hover{border-color:var(--risk);color:var(--risk)}
  button:disabled{opacity:.45;cursor:default}
  select{font:inherit;font-size:.79rem;padding:.33rem .4rem;border-radius:5px;
    border:1px solid var(--rule);background:var(--card);color:var(--ink2)}
  .empty,.err{padding:2.5rem 1rem;text-align:center;color:var(--ink3)}
  .err{color:var(--risk)}
  #toast{position:fixed;left:50%;bottom:1.25rem;transform:translateX(-50%);
    background:var(--ink);color:var(--bg);padding:.6rem 1rem;border-radius:6px;
    font-size:.83rem;max-width:min(92vw,40rem);overflow-wrap:anywhere;
    opacity:0;pointer-events:none;transition:opacity .18s}
  #toast.on{opacity:1}
</style></head><body>
<div class="wrap">
  <header>
    <h1>sendup</h1>
    <div class="totals" id="totals"></div>
  </header>
  <div id="list"><div class="empty">loading…</div></div>
</div>
<div id="toast"></div>
<script>
const BASE = '__BASE__'
const list = document.getElementById('list')
const totalsEl = document.getElementById('totals')
const toastEl = document.getElementById('toast')
let timer

function toast(msg) {
  toastEl.textContent = msg
  toastEl.classList.add('on')
  clearTimeout(timer)
  timer = setTimeout(() => toastEl.classList.remove('on'), 4200)
}

async function api(path, body) {
  const res = await fetch('/api/' + path, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-sendup': '1' },
    body: JSON.stringify(body || {}),
  })
  const data = await res.json().catch(() => ({ error: 'bad response' }))
  if (!res.ok) throw new Error(data.error || res.status)
  return data
}

const bytes = (n) =>
  n < 1024 ? n + ' B'
  : n < 1048576 ? (n / 1024).toFixed(1) + ' KB'
  : (n / 1048576).toFixed(1) + ' MB'

const esc = (s) => String(s).replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))

function card(s) {
  const el = document.createElement('div')
  el.className = 'card'
  const url = 'https://' + s.slug + '.' + BASE + '/'
  const when = s.created ? s.created.slice(0, 10) : '?'
  const exp = s.expires ? ' · expires ' + s.expires.slice(0, 10) : ''
  el.innerHTML =
    '<div class="who">' +
      '<div class="name"><a href="' + esc(url) + '" target="_blank" rel="noopener">' +
        esc(s.slug) + '</a><span class="pill p-' + s.access + '">' + s.access + '</span></div>' +
      (s.title ? '<div class="title">' + esc(s.title) + '</div>' : '') +
      '<div class="facts"><span>' + when + exp + '</span><span>' + s.files +
        ' file' + (s.files === 1 ? '' : 's') + '</span><span>' + bytes(s.bytes) + '</span>' +
        (s.src ? '<span>' + esc(s.src) + '</span>' : '') + '</div>' +
    '</div>'

  const acts = document.createElement('div')
  acts.className = 'acts'

  const sel = document.createElement('select')
  for (const [v, label] of [['open', 'open'], ['link', 'signed link'], ['auth', 'password']]) {
    const o = document.createElement('option')
    o.value = v
    o.textContent = label
    if (s.access === v || (s.access === 'both' && v === 'auth')) o.selected = true
    sel.append(o)
  }
  sel.onchange = () => run(sel, async () => {
    const r = await api('access', { slug: s.slug, mode: sel.value })
    toast(r.password ? 'password set — ' + r.password : 'access set to ' + sel.value)
    await load()
  })
  acts.append(sel)

  if (s.access === 'link' || s.access === 'both') {
    acts.append(btn('copy link', () => run(null, async () => {
      const r = await api('link', { slug: s.slug })
      await copy(r.url)
    })))
    acts.append(btn('7d link', () => run(null, async () => {
      const r = await api('link', { slug: s.slug, days: 7 })
      await copy(r.url)
    })))
    acts.append(btn('revoke', () => run(null, async () => {
      if (!confirm('Kill every link issued for ' + s.slug + '?')) return
      await api('rotate', { slug: s.slug })
      toast('links revoked for ' + s.slug)
    })))
  }

  const del = btn('delete', () => run(null, async () => {
    if (!confirm('Delete ' + s.slug + ' and all its files? This cannot be undone.')) return
    const r = await api('delete', { slug: s.slug })
    toast('deleted ' + s.slug + ' (' + r.files + ' files)')
    await load()
  }))
  del.className = 'danger'
  acts.append(del)

  el.append(acts)
  return el
}

function btn(label, fn) {
  const b = document.createElement('button')
  b.textContent = label
  b.onclick = fn
  return b
}

async function copy(text) {
  try {
    await navigator.clipboard.writeText(text)
    toast('copied: ' + text)
  } catch {
    toast(text)
  }
}

async function run(el, fn) {
  if (el) el.disabled = true
  try {
    await fn()
  } catch (e) {
    toast('failed: ' + e.message)
  } finally {
    if (el) el.disabled = false
  }
}

async function load() {
  try {
    const d = await api('list')
    totalsEl.innerHTML =
      '<span>' + d.totals.sites + ' sites</span>' +
      '<span>' + d.totals.files + ' files</span>' +
      '<span>' + bytes(d.totals.bytes) + ' of 1 GB</span>'
    list.textContent = ''
    if (!d.sites.length) {
      list.innerHTML = '<div class="empty">nothing published yet</div>'
      return
    }
    for (const s of d.sites) list.append(card(s))
  } catch (e) {
    list.innerHTML = '<div class="err">could not load: ' + esc(e.message) + '</div>'
  }
}

load()
</script></body></html>`
