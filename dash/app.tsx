import { useCallback, useEffect, useState } from 'react'
import {
  Badge,
  Banner,
  Button,
  Dialog,
  Empty,
  Input,
  Loader,
  Meter,
  Select,
  Table,
  Text,
  useKumoToastManager,
} from '@cloudflare/kumo'
import {
  ArrowClockwiseIcon,
  ArrowsClockwiseIcon,
  CloudArrowUpIcon,
  LinkIcon,
  MoonIcon,
  PencilSimpleIcon,
  SunIcon,
  TrashIcon,
} from '@phosphor-icons/react'

const BASE: string = (window as any).__SENDUP_BASE__
const GB = 1024 ** 3

type Access = 'open' | 'link' | 'auth' | 'both'

type Site = {
  slug: string
  title: string
  src: string
  created: string
  expires: string
  files: number
  bytes: number
  access: Access
}

type Totals = { sites: number; files: number; bytes: number }

async function api<T>(path: string, body: unknown = {}): Promise<T> {
  const res = await fetch(`/api/${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-sendup': '1' },
    body: JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({ error: `HTTP ${res.status}` }))
  if (!res.ok) throw new Error((data as { error?: string }).error ?? `HTTP ${res.status}`)
  return data as T
}

function formatBytes(n: number) {
  if (n < 1024) return `${n} B`
  if (n < 1024 ** 2) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 ** 2).toFixed(1)} MB`
}

// Cloudflare KV list is eventually consistent, so for up to a minute after a change the
// server still reports the old world — including across a page reload, which throws away
// any in-memory correction. Park the change in sessionStorage and replay it over each
// response until the server catches up.
type Pending =
  | { type: 'delete'; slug: string; at: number }
  | { type: 'rename'; slug: string; to: string; at: number }

const PENDING_KEY = 'sendup:pending'
const PENDING_TTL = 150_000

function readPending(): Pending[] {
  try {
    const raw = sessionStorage.getItem(PENDING_KEY)
    if (!raw) return []
    const now = Date.now()
    return (JSON.parse(raw) as Pending[]).filter((p) => now - p.at < PENDING_TTL)
  } catch {
    return []
  }
}

function writePending(list: Pending[]) {
  try {
    if (list.length) sessionStorage.setItem(PENDING_KEY, JSON.stringify(list))
    else sessionStorage.removeItem(PENDING_KEY)
  } catch {
    /* private mode, quota — the in-memory update still applies */
  }
}

const addPending = (p: Pending) => writePending([...readPending(), p])

// Replays outstanding changes over a server response and forgets the ones it has caught
// up on. Returns the corrected list plus how many are still outstanding.
export function reconcile(sites: Site[]): { sites: Site[]; waiting: number; syncing: string[] } {
  const keep: Pending[] = []
  const syncing: string[] = []
  let out = sites

  for (const p of readPending()) {
    const hasOld = out.some((s) => s.slug === p.slug)
    if (p.type === 'delete') {
      if (!hasOld) continue // server agrees it is gone
      out = out.filter((s) => s.slug !== p.slug)
    } else {
      const hasNew = out.some((s) => s.slug === p.to)
      if (!hasOld && hasNew) continue // server agrees on the new name
      // mid-propagation the list can carry both; drop the stale one either way
      out = hasNew
        ? out.filter((s) => s.slug !== p.slug)
        : out.map((s) => (s.slug === p.slug ? { ...s, slug: p.to } : s))
    }
    keep.push(p)
    // a deleted row is not on screen, so only renames get a per-row marker
    if (p.type === 'rename') syncing.push(p.to)
  }

  writePending(keep)
  return { sites: out, waiting: keep.length, syncing }
}

// Dark is the default; the choice sticks per browser. The initial value is applied by a
// boot script in the document head so there is no flash before React mounts.
function useColorMode() {
  const [mode, setMode] = useState<'dark' | 'light'>(
    () => (document.documentElement.dataset.mode === 'light' ? 'light' : 'dark')
  )
  const toggle = useCallback(() => {
    setMode((prev) => {
      const next = prev === 'dark' ? 'light' : 'dark'
      document.documentElement.dataset.mode = next
      try {
        localStorage.setItem('sendup:mode', next)
      } catch {
        /* private mode — the choice just will not persist */
      }
      return next
    })
  }, [])
  return { mode, toggle }
}

const totalsOf = (sites: Site[]): Totals => ({
  sites: sites.length,
  files: sites.reduce((a, s) => a + s.files, 0),
  bytes: sites.reduce((a, s) => a + s.bytes, 0),
})

const ACCESS_LABEL: Record<Access, string> = {
  open: 'anyone with the URL',
  link: 'signed link',
  auth: 'password',
  both: 'link or password',
}

function DeleteDialog({ site, onDeleted }: { site: Site; onDeleted: () => void }) {
  const toast = useKumoToastManager()
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)

  async function confirm() {
    setBusy(true)
    try {
      const r = await api<{ files: number }>('delete', { slug: site.slug })
      toast.add({ title: `Deleted ${site.slug}`, description: `${r.files} files removed` })
      setOpen(false)
      // drop it locally rather than refetching: KV list is eventually consistent and
      // would happily hand back the key we just deleted
      onDeleted()
    } catch (e) {
      toast.add({ title: 'Delete failed', description: (e as Error).message, type: 'error' })
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog.Root open={open} onOpenChange={setOpen}>
      <Dialog.Trigger
        render={(p) => (
          <Button
            {...p}
            variant="secondary-destructive"
            shape="square"
            title="Delete site"
            aria-label={`Delete ${site.slug}`}
          >
            <TrashIcon />
          </Button>
        )}
      />
      <Dialog size="base" className="p-6">
        <Dialog.Title render={<Text as="h2" variant="heading3" />}>Delete {site.slug}?</Dialog.Title>
        <Dialog.Description render={<Text variant="secondary" />} className="mt-1.5">
          Removes {site.files} file{site.files === 1 ? '' : 's'} and the URL stops working. This
          cannot be undone.
        </Dialog.Description>
        <div className="mt-6 flex justify-end gap-2">
          <Dialog.Close render={(p) => <Button {...p} variant="secondary">Cancel</Button>} />
          <Button variant="destructive" loading={busy} onClick={confirm}>
            Delete
          </Button>
        </div>
      </Dialog>
    </Dialog.Root>
  )
}

function RenameDialog({ site, onRenamed }: { site: Site; onRenamed: (to: string) => void }) {
  const toast = useKumoToastManager()
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [value, setValue] = useState(site.slug)
  const [problem, setProblem] = useState('')

  function start(next: boolean) {
    setOpen(next)
    if (next) {
      setValue(site.slug)
      setProblem('')
    }
  }

  async function submit() {
    const to = value.trim().toLowerCase()
    if (to === site.slug) return setProblem('That is already its name.')
    if (!/^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$/.test(to)) {
      return setProblem('Lowercase letters, digits and hyphens, 2–63 characters.')
    }

    setBusy(true)
    setProblem('')
    try {
      const r = await api<{ slug: string; linksInvalidated: boolean }>('rename', {
        slug: site.slug,
        to,
      })
      toast.add({
        title: `Now at ${r.slug}.${BASE}`,
        description: r.linksInvalidated
          ? 'The old URL and every signed link issued for it have stopped working.'
          : 'The old URL has stopped working.',
        type: 'success',
      })
      setOpen(false)
      onRenamed(r.slug)
    } catch (e) {
      setProblem((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog.Root open={open} onOpenChange={start}>
      <Dialog.Trigger
        render={(p) => (
          <Button
            {...p}
            shape="square"
            title="Change subdomain"
            aria-label={`Change subdomain for ${site.slug}`}
          >
            <PencilSimpleIcon />
          </Button>
        )}
      />
      <Dialog size="lg" className="p-6">
        <Dialog.Title render={<Text as="h2" variant="heading3" />}>Change subdomain</Dialog.Title>
        <Dialog.Description render={<Text variant="secondary" />} className="mt-1.5">
          Moves {site.files} file{site.files === 1 ? '' : 's'}. The current URL stops working
          immediately{site.access === 'link' || site.access === 'both'
            ? ', and every signed link already issued for this site dies with it'
            : ''}
          .
        </Dialog.Description>

        <div className="mt-4">
          <Input
            autoFocus
            name="subdomain"
            label="Subdomain"
            description={`https://${value.trim().toLowerCase() || '…'}.${BASE}/`}
            error={problem || undefined}
            value={value}
            disabled={busy}
            spellCheck={false}
            autoCapitalize="off"
            autoCorrect="off"
            autoComplete="off"
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void submit()
            }}
          />
        </div>

        <div className="mt-6 flex justify-end gap-2">
          <Dialog.Close render={(p) => <Button {...p} variant="secondary">Cancel</Button>} />
          <Button variant="primary" loading={busy} onClick={submit}>
            Rename
          </Button>
        </div>
      </Dialog>
    </Dialog.Root>
  )
}

function Row({
  site,
  syncing,
  onDeleted,
  onAccess,
  onRenamed,
}: {
  site: Site
  syncing: boolean
  onDeleted: () => void
  onAccess: (access: Access) => void
  onRenamed: (to: string) => void
}) {
  const toast = useKumoToastManager()
  const [busy, setBusy] = useState(false)
  const url = `https://${site.slug}.${BASE}/`
  const linkable = site.access === 'link' || site.access === 'both'

  async function guard(fn: () => Promise<void>) {
    setBusy(true)
    try {
      await fn()
    } catch (e) {
      toast.add({ title: 'Failed', description: (e as Error).message, type: 'error' })
    } finally {
      setBusy(false)
    }
  }

  const setAccess = (mode: string | null) =>
    guard(async () => {
      if (!mode || mode === site.access) return
      const r = await api<{ password: string | null }>('access', { slug: site.slug, mode })
      toast.add({
        title: `${site.slug} — ${ACCESS_LABEL[mode as Access]}`,
        description: r.password ? `Login: ${r.password} (shown once)` : undefined,
        type: 'success',
      })
      onAccess(mode as Access)
    })

  const copyLink = (days?: number) =>
    guard(async () => {
      const r = await api<{ url: string }>('link', { slug: site.slug, days })
      try {
        await navigator.clipboard.writeText(r.url)
        toast.add({
          title: days ? `${days}-day link copied` : 'Link copied',
          description: r.url,
          type: 'success',
        })
      } catch {
        toast.add({ title: 'Copy this link', description: r.url })
      }
    })

  const revoke = () =>
    guard(async () => {
      await api('rotate', { slug: site.slug })
      toast.add({ title: `Links revoked for ${site.slug}`, type: 'warning' })
    })

  return (
    <Table.Row>
      <Table.Cell>
        <div className="flex flex-col gap-0.5">
          <a
            href={url}
            target="_blank"
            rel="noopener noreferrer"
            className="font-medium break-all hover:underline"
          >
            <Text>{site.slug}</Text>
          </a>
          {site.title && (
            <Text variant="secondary" size="sm" className="break-all">
              {site.title}
            </Text>
          )}
          {syncing && (
            <span className="flex items-center gap-1.5">
              <span className="h-lh flex items-center">
                <Loader size="sm" />
              </span>
              <Text variant="secondary" size="xs">
                renamed — edge still catching up
              </Text>
            </span>
          )}
        </div>
      </Table.Cell>

      <Table.Cell>
        <Badge>{ACCESS_LABEL[site.access]}</Badge>
      </Table.Cell>

      <Table.Cell className="whitespace-nowrap">
        <Text variant="secondary">
          {site.files} · {formatBytes(site.bytes)}
        </Text>
      </Table.Cell>

      <Table.Cell className="whitespace-nowrap">
        <Text variant="secondary">
          {site.created ? site.created.slice(0, 10) : '—'}
        </Text>
        {site.expires && (
          <Text variant="secondary" size="xs">
            expires {site.expires.slice(0, 10)}
          </Text>
        )}
      </Table.Cell>

      <Table.Cell>
        <div className="flex items-center justify-end gap-2">
          <Select
            aria-label={`Access for ${site.slug}`}
            className="w-[11.5rem]"
            value={site.access === 'both' ? 'auth' : site.access}
            disabled={busy}
            onValueChange={setAccess}
            items={{ open: 'Open', link: 'Signed link', auth: 'Password' }}
          />
          {linkable && (
            <>
              <Button
                    shape="square"
                title="Copy share link"
                aria-label={`Copy share link for ${site.slug}`}
                disabled={busy}
                onClick={() => copyLink()}
              >
                <LinkIcon />
              </Button>
              <Button aria-label={`Copy 7-day link for ${site.slug}`} disabled={busy} onClick={() => copyLink(7)}>
                7d
              </Button>
              <Button
                    shape="square"
                title="Revoke every issued link"
                aria-label={`Revoke every link for ${site.slug}`}
                disabled={busy}
                onClick={revoke}
              >
                <ArrowClockwiseIcon />
              </Button>
            </>
          )}
          <RenameDialog site={site} onRenamed={onRenamed} />
          <DeleteDialog site={site} onDeleted={onDeleted} />
        </div>
      </Table.Cell>
    </Table.Row>
  )
}

export function App() {
  const [sites, setSites] = useState<Site[] | null>(null)
  const [waiting, setWaiting] = useState(0)
  const [syncing, setSyncing] = useState<string[]>([])
  const [error, setError] = useState('')
  const { mode, toggle } = useColorMode()
  const totals = totalsOf(sites ?? [])

  const load = useCallback(async () => {
    try {
      const d = await api<{ sites: Site[] }>('list')
      const fixed = reconcile(d.sites)
      setSites(fixed.sites)
      setWaiting(fixed.waiting)
      setSyncing(fixed.syncing)
      setError('')
    } catch (e) {
      setError((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // Keep polling while a change is still propagating, so the banner clears itself.
  useEffect(() => {
    if (!waiting) return
    const t = setTimeout(() => void load(), 15_000)
    return () => clearTimeout(t)
  }, [waiting, load])

  const dropSite = useCallback((slug: string) => {
    addPending({ type: 'delete', slug, at: Date.now() })
    setSites((prev) => prev && prev.filter((s) => s.slug !== slug))
    setWaiting((n) => n + 1)
  }, [])

  const setAccess = useCallback((slug: string, access: Access) => {
    // access lives on a single key, which is read-your-write consistent — no replay needed
    setSites((prev) => prev && prev.map((s) => (s.slug === slug ? { ...s, access } : s)))
  }, [])

  const renameSite = useCallback((slug: string, to: string) => {
    addPending({ type: 'rename', slug, to, at: Date.now() })
    setSites((prev) => prev && prev.map((s) => (s.slug === slug ? { ...s, slug: to } : s)))
    setWaiting((n) => n + 1)
    setSyncing((prev) => [...prev, to])
  }, [])

  return (
    <div className="mx-auto max-w-6xl px-4 py-8">
      <header className="mb-6 flex flex-wrap items-start justify-between gap-x-8 gap-y-4 border-b border-kumo pb-5">
        {/* title and its subtitle belong together, so they sit tighter than the row gap */}
        <div className="grid gap-1">
          <Text as="h1" variant="heading3">
            sendup
          </Text>
          <Text variant="secondary">
            {totals.sites} site{totals.sites === 1 ? '' : 's'} · {totals.files} files on {BASE}
          </Text>
        </div>

        <div className="flex items-center gap-3">
          <div className="w-56">
            <Meter
              label="Storage"
              value={totals.bytes}
              max={GB}
              customValue={`${formatBytes(totals.bytes)} of 1 GB`}
            />
          </div>
          <Button
            shape="square"
            aria-label="Reload from the server"
            title="Reload"
            onClick={() => void load()}
          >
            <ArrowsClockwiseIcon />
          </Button>
          <Button
            shape="square"
            aria-label={mode === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
            title={mode === 'dark' ? 'Light theme' : 'Dark theme'}
            onClick={toggle}
          >
            {mode === 'dark' ? <SunIcon /> : <MoonIcon />}
          </Button>
        </div>
      </header>

      {waiting > 0 && (
        <Banner
          className="mb-4"
          icon={<CloudArrowUpIcon />}
          title={`${waiting} change${waiting === 1 ? '' : 's'} still reaching the edge`}
          description="Cloudflare KV takes up to a minute to agree across regions. What you see here is already correct, and it will settle on its own."
        />
      )}

      {error && (
        <Empty
          title="Could not load sites"
          description={error}
          contents={<Button onClick={() => void load()}>Retry</Button>}
        />
      )}

      {!error && sites === null && (
        <div className="flex justify-center py-16">
          <Loader />
        </div>
      )}

      {!error && sites?.length === 0 && (
        <Empty
          icon={<CloudArrowUpIcon size={48} />}
          title="Nothing published yet"
          description="Publish a page and it will show up here."
          commandLine="sendup publish index.html"
        />
      )}

      {!error && sites !== null && sites.length > 0 && (
        <div className="overflow-x-auto">
        <Table>
          <Table.Header>
            <Table.Row>
              <Table.Head>Site</Table.Head>
              <Table.Head>Access</Table.Head>
              <Table.Head>Size</Table.Head>
              <Table.Head>Published</Table.Head>
              <Table.Head className="text-right">
                Actions
              </Table.Head>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {sites.map((s) => (
              <Row
                key={s.slug}
                site={s}
                syncing={syncing.includes(s.slug)}
                onDeleted={() => dropSite(s.slug)}
                onAccess={(a) => setAccess(s.slug, a)}
                onRenamed={(to) => renameSite(s.slug, to)}
              />
            ))}
          </Table.Body>
        </Table>
        </div>
      )}
    </div>
  )
}
