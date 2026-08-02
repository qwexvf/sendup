import { useCallback, useEffect, useState } from 'react'
import {
  Badge,
  Button,
  Dialog,
  Empty,
  Loader,
  Meter,
  Select,
  Table,
  useKumoToastManager,
} from '@cloudflare/kumo'
import {
  ArrowClockwiseIcon,
  CloudArrowUpIcon,
  LinkIcon,
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

const ACCESS_LABEL: Record<Access, string> = {
  open: 'anyone with the URL',
  link: 'signed link',
  auth: 'password',
  both: 'link or password',
}

const ACCESS_VARIANT: Record<Access, 'orange' | 'teal' | 'green' | 'blue'> = {
  open: 'orange',
  link: 'teal',
  auth: 'green',
  both: 'blue',
}

function DeleteDialog({ site, onDone }: { site: Site; onDone: () => void }) {
  const toast = useKumoToastManager()
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)

  async function confirm() {
    setBusy(true)
    try {
      const r = await api<{ files: number }>('delete', { slug: site.slug })
      toast.add({ title: `Deleted ${site.slug}`, description: `${r.files} files removed` })
      setOpen(false)
      onDone()
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
            size="xs"
            variant="secondary-destructive"
            shape="square"
            title="Delete site"
            aria-label={`Delete ${site.slug}`}
          >
            <TrashIcon />
          </Button>
        )}
      />
      <Dialog size="sm" className="p-6">
        <Dialog.Title className="text-lg font-semibold">Delete {site.slug}?</Dialog.Title>
        <Dialog.Description className="mt-2 text-kumo-subtle">
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

function Row({ site, onChanged }: { site: Site; onChanged: () => void }) {
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
      onChanged()
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
            {site.slug}
          </a>
          {site.title && <span className="text-sm text-kumo-subtle break-all">{site.title}</span>}
        </div>
      </Table.Cell>

      <Table.Cell>
        <Badge variant={ACCESS_VARIANT[site.access]}>{ACCESS_LABEL[site.access]}</Badge>
      </Table.Cell>

      <Table.Cell className="whitespace-nowrap tabular-nums text-kumo-subtle">
        {site.files} · {formatBytes(site.bytes)}
      </Table.Cell>

      <Table.Cell className="whitespace-nowrap tabular-nums text-kumo-subtle">
        {site.created ? site.created.slice(0, 10) : '—'}
        {site.expires && <div className="text-xs">expires {site.expires.slice(0, 10)}</div>}
      </Table.Cell>

      <Table.Cell sticky="right">
        <div className="flex items-center justify-end gap-1.5">
          <Select
            aria-label={`Access for ${site.slug}`}
            size="xs"
            className="w-[9.5rem]"
            value={site.access === 'both' ? 'auth' : site.access}
            disabled={busy}
            onValueChange={setAccess}
            items={{ open: 'Open', link: 'Signed link', auth: 'Password' }}
          />
          {linkable && (
            <>
              <Button
                size="xs"
                shape="square"
                title="Copy share link"
                aria-label={`Copy share link for ${site.slug}`}
                disabled={busy}
                onClick={() => copyLink()}
              >
                <LinkIcon />
              </Button>
              <Button size="xs" aria-label={`Copy 7-day link for ${site.slug}`} disabled={busy} onClick={() => copyLink(7)}>
                7d
              </Button>
              <Button
                size="xs"
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
          <DeleteDialog site={site} onDone={onChanged} />
        </div>
      </Table.Cell>
    </Table.Row>
  )
}

export function App() {
  const [sites, setSites] = useState<Site[] | null>(null)
  const [totals, setTotals] = useState<Totals>({ sites: 0, files: 0, bytes: 0 })
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    try {
      const d = await api<{ sites: Site[]; totals: Totals }>('list')
      setSites(d.sites)
      setTotals(d.totals)
      setError('')
    } catch (e) {
      setError((e as Error).message)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  return (
    <div className="mx-auto max-w-6xl px-4 py-8">
      <header className="mb-6 flex flex-wrap items-end justify-between gap-4 border-b border-kumo pb-4">
        <div>
          <h1 className="text-xl font-semibold tracking-tight">sendup</h1>
          <p className="text-sm text-kumo-subtle">
            {totals.sites} site{totals.sites === 1 ? '' : 's'} · {totals.files} files on {BASE}
          </p>
        </div>
        <div className="w-full sm:w-64">
          <Meter
            label="Storage"
            value={totals.bytes}
            max={GB}
            customValue={`${formatBytes(totals.bytes)} of 1 GB`}
          />
        </div>
      </header>

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
        <Table>
          <Table.Header>
            <Table.Row>
              <Table.Head>Site</Table.Head>
              <Table.Head>Access</Table.Head>
              <Table.Head>Size</Table.Head>
              <Table.Head>Published</Table.Head>
              <Table.Head sticky="right" className="text-right">
                Actions
              </Table.Head>
            </Table.Row>
          </Table.Header>
          <Table.Body>
            {sites.map((s) => (
              <Row key={s.slug} site={s} onChanged={() => void load()} />
            ))}
          </Table.Body>
        </Table>
      )}
    </div>
  )
}
