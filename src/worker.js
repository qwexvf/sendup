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
  h.set('cache-control', 'public, max-age=60, must-revalidate')
  if (meta && meta.etag) h.set('etag', meta.etag)
  return h
}

const page = (code, msg) =>
  new Response(
    `<!doctype html><meta charset=utf-8><title>${code}</title>` +
      `<style>body{font:16px/1.6 system-ui,sans-serif;display:grid;place-items:center;height:100vh;margin:0;background:#0b0b0c;color:#e6e6e6}` +
      `div{text-align:center}h1{font-size:3rem;margin:0 0 .25rem;font-weight:600}p{opacity:.6;margin:0}</style>` +
      `<div><h1>${code}</h1><p>${msg}</p></div>`,
    { status: code, headers: { 'content-type': 'text/html; charset=utf-8', 'x-robots-tag': 'noindex' } }
  )

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
      const custom = await env.SITES.getWithMetadata(`${slug}:/404.html`, { type: 'arrayBuffer' })
      if (custom.value !== null) {
        return new Response(custom.value, { status: 404, headers: headers('/404.html', custom.metadata) })
      }
      const exists = await env.SITES.get(`${slug}:/index.html`, { type: 'stream' })
      return page(404, exists ? 'no such page' : 'no such site')
    }

    const h = headers(path, hit.metadata)
    const etag = h.get('etag')
    if (etag && request.headers.get('if-none-match') === etag) {
      return new Response(null, { status: 304, headers: h })
    }

    return new Response(request.method === 'HEAD' ? null : hit.value, { status: 200, headers: h })
  },
}
