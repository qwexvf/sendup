// serves static sites out of KV, keyed by subdomain slug.
// key format: "<slug>:<absolute-path>", e.g. "a3f9c1d2e4b60718:/index.html"

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

function contentType(path, meta) {
  if (meta && meta.ct) return meta.ct
  const ext = path.slice(path.lastIndexOf('.') + 1).toLowerCase()
  return TYPES[ext] || 'application/octet-stream'
}

function headers(path, meta, extra) {
  const h = new Headers(extra)
  h.set('content-type', contentType(path, meta))
  // unlisted, not secret — keep it out of search indexes
  h.set('x-robots-tag', 'noindex, nofollow, noarchive')
  h.set('x-content-type-options', 'nosniff')
  h.set('referrer-policy', 'no-referrer')
  if (meta && (meta.auth || meta.link)) {
    // never let a shared cache hold a credentialed response
    h.set('cache-control', 'private, no-store')
    h.set('vary', 'Authorization, Cookie')
  } else {
    h.set('cache-control', 'public, max-age=60, must-revalidate')
  }
  if (meta && meta.etag) h.set('etag', meta.etag)
  return h
}

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

// auth = { salt, hash } — hash is sha256(salt + "user:pass"), both hex
async function authorized(request, auth) {
  if (!auth || typeof auth.salt !== 'string') return false
  const expected = hexToBytes(auth.hash)
  if (!expected) return false

  const header = request.headers.get('authorization') || ''
  if (!header.startsWith('Basic ')) return false

  let creds
  try {
    // atob gives latin1 bytes; recover the original utf-8 so non-ascii passwords work
    const raw = atob(header.slice(6).trim())
    const bytes = Uint8Array.from(raw, (c) => c.charCodeAt(0))
    creds = new TextDecoder().decode(bytes)
  } catch {
    return false
  }

  const digest = new Uint8Array(
    await crypto.subtle.digest('SHA-256', new TextEncoder().encode(auth.salt + creds))
  )
  if (digest.length !== expected.length) return false
  // plain loop rather than crypto.subtle.timingSafeEqual — that one is a Workers
  // extension and this stays runnable under plain node for the tests
  let diff = 0
  for (let i = 0; i < digest.length; i++) diff |= digest[i] ^ expected[i]
  return diff === 0
}

function constantTimeEqual(a, b) {
  if (a.length !== b.length) return false
  let diff = 0
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i]
  return diff === 0
}

// token = "<exp>.<sig>", exp is unix seconds (0 = never), sig is the first 16 bytes of
// HMAC-SHA256(secret, "<slug>:<exp>") in hex. The expiry is inside the signed message,
// so a recipient cannot extend their own link.
async function validToken(token, slug, secretHex) {
  if (typeof token !== 'string') return false
  const dot = token.indexOf('.')
  if (dot < 1) return false

  const exp = Number(token.slice(0, dot))
  if (!Number.isSafeInteger(exp) || exp < 0) return false
  if (exp !== 0 && exp * 1000 < Date.now()) return false

  const sig = hexToBytes(token.slice(dot + 1))
  const secret = hexToBytes(secretHex)
  if (!sig || !secret || sig.length !== 16) return false

  const key = await crypto.subtle.importKey('raw', secret, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign'])
  const mac = new Uint8Array(
    await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(`${slug}:${exp}`))
  )
  return constantTimeEqual(sig, mac.slice(0, 16))
}

function cookieValue(request, name) {
  const jar = request.headers.get('cookie')
  if (!jar) return null
  for (const part of jar.split(';')) {
    const eq = part.indexOf('=')
    if (eq > 0 && part.slice(0, eq).trim() === name) return part.slice(eq + 1).trim()
  }
  return null
}

const COOKIE = 'sendup_link'

// Returns "ok" when the caller already holds a cookie, "grant" when they arrived with a
// fresh ?k= token and should be handed one, or false.
async function linkAccess(request, url, slug, secretHex) {
  const q = url.searchParams.get('k')
  if (q && (await validToken(q, slug, secretHex))) return 'grant'
  const c = cookieValue(request, COOKIE)
  if (c && (await validToken(c, slug, secretHex))) return 'ok'
  return false
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

const unauthorized = () =>
  new Response('401 — authentication required', {
    status: 401,
    headers: {
      'www-authenticate': 'Basic realm="sendup", charset="UTF-8"',
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

const page = (code, msg) =>
  new Response(
    `<!doctype html><meta charset=utf-8><title>${code}</title>` +
      `<style>body{font:16px/1.6 system-ui,sans-serif;display:grid;place-items:center;height:100vh;margin:0;background:#0b0b0c;color:#e6e6e6}` +
      `div{text-align:center}h1{font-size:3rem;margin:0 0 .25rem;font-weight:600}p{opacity:.6;margin:0}</style>` +
      `<div><h1>${code}</h1><p>${msg}</p></div>`,
    { status: code, headers: { 'content-type': 'text/html; charset=utf-8', 'x-robots-tag': 'noindex' } }
  )

// Decides whether a request may see a site. A site can carry a signed-link secret, a
// basic-auth hash, both, or neither; holding either credential is enough.
// Returns null to allow, a Response to stop.
async function gate(request, url, slug, meta) {
  if (!meta || (!meta.link && !meta.auth)) return null

  if (meta.link) {
    const access = await linkAccess(request, url, slug, meta.link)
    if (access === 'grant') return grantCookie(url, url.searchParams.get('k'))
    if (access === 'ok') return null
  }
  if (meta.auth && (await authorized(request, meta.auth))) return null

  // a password box is only meaningful if a password would actually work
  return meta.auth ? unauthorized() : forbidden()
}

export default {
  async fetch(request, env) {
    if (request.method !== 'GET' && request.method !== 'HEAD') {
      return page(405, 'method not allowed')
    }

    const url = new URL(request.url)
    const host = url.hostname.toLowerCase()
    const base = (env.BASE_DOMAIN || 'example.com').toLowerCase()

    if (!host.endsWith('.' + base)) return page(404, 'unknown host')
    const slug = host.slice(0, -(base.length + 1))
    if (!slug || slug.includes('.')) return page(404, 'unknown host')

    let path = url.pathname
    try {
      path = decodeURIComponent(path)
    } catch {
      return page(400, 'bad path')
    }
    if (!path.startsWith('/')) path = '/' + path
    if (path.includes('..')) return page(400, 'bad path')
    if (path.endsWith('/')) path += 'index.html'

    let hit = await env.SITES.getWithMetadata(`${slug}:${path}`, { type: 'arrayBuffer' })

    // extensionless request -> try it as a directory
    if (hit.value === null && !path.slice(path.lastIndexOf('/')).includes('.')) {
      hit = await env.SITES.getWithMetadata(`${slug}:${path}/index.html`, { type: 'arrayBuffer' })
      if (hit.value !== null) path += '/index.html'
    }

    if (hit.value === null) {
      // a miss carries no metadata, so fall back to the site record — otherwise an
      // unauthenticated caller could probe which paths exist on a protected site
      const index = await env.SITES.getWithMetadata(`__index:${slug}`, { type: 'text' })
      if (index.metadata === null) return page(404, 'no such site')
      const blocked = await gate(request, url, slug, index.metadata)
      if (blocked) return blocked

      const custom = await env.SITES.getWithMetadata(`${slug}:/404.html`, { type: 'arrayBuffer' })
      if (custom.value !== null) {
        return new Response(custom.value, { status: 404, headers: headers('/404.html', custom.metadata) })
      }
      return page(404, 'no such page')
    }

    const blocked = await gate(request, url, slug, hit.metadata)
    if (blocked) return blocked

    const h = headers(path, hit.metadata)
    const etag = h.get('etag')
    if (etag && request.headers.get('if-none-match') === etag) {
      return new Response(null, { status: 304, headers: h })
    }

    return new Response(request.method === 'HEAD' ? null : hit.value, { status: 200, headers: h })
  },
}
