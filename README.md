# sendup

Publish a static HTML file to a URL on **your own domain**, in about a second.

```console
$ sendup publish report.html
https://a3f9c1d2e4b60718.example.com/
  1 file, 36.3 KB (new site)
```

It's the "share this page with a link" part of Claude Artifacts, on infrastructure you
own — your domain, your Cloudflare account, no third party in the loop. Single-file
pages, whole directories, drafts you want gone in a week, client documents behind a
password.

Runs entirely on Cloudflare's free tier.

## How it works

One Worker sits on a wildcard route. It reads the subdomain off the `Host` header and
serves the matching bytes out of Workers KV.

```
*.example.com  AAAA 100:: (proxied)
      |
      +-- worker route *.example.com/*
                |
                +-- KV  "<slug>:/index.html"  ->  bytes
```

The zone holds **one** DNS record no matter how many pages you publish, so the free
plan's 200-records-per-zone cap never applies. Cloudflare Pages would need a project and
a DNS record per site; this doesn't.

There is no build step, no server, and no database. Whatever you hand it is what gets
served.

## Install

Requires [Bun](https://bun.sh), Go, and a domain on Cloudflare. No Node, no npm.

```bash
git clone https://github.com/<you>/sendup && cd sendup
bun install
(cd cli && go build -o sendup .) && ln -s "$PWD/cli/sendup" ~/.local/bin/sendup
```

Create a **user-owned** API token at
<https://dash.cloudflare.com/profile/api-tokens> → Create Token → Custom token:

| Scope   | Permission         | Level | Needed for                          |
| ------- | ------------------ | ----- | ----------------------------------- |
| Account | Workers KV Storage | Edit  | `init`, `publish`, `rm`             |
| Account | Workers Scripts    | Edit  | `wrangler deploy`                   |
| Zone    | Workers Routes     | Edit  | `wrangler deploy` — binds the route |
| Zone    | Zone               | Read  | `init` (zone + account lookup)      |
| Zone    | DNS                | Edit  | wildcard record — optional          |

Scope Zone Resources to your domain. Account-owned tokens (Manage Account → API Tokens)
**cannot hold zone permissions at all** — if the editor shows no Zone section, that's
why.

Only the first row is needed day to day. See **[docs/PERMISSIONS.md](docs/PERMISSIONS.md)**
for the per-command breakdown, how to avoid granting DNS access at all, and how to swap
in a narrower publish-only token afterwards.

```bash
sendup init --domain example.com --token <TOKEN>
CLOUDFLARE_API_TOKEN=<TOKEN> bun run deploy
```

`init` creates the KV namespace, adds the wildcard DNS record, and writes
`~/.config/sendup/config.json` (mode 0600). It also generates **`wrangler.jsonc`** from
`wrangler.template.jsonc` with your domain and namespace filled in — the generated file
is gitignored, so no one's domain ends up in the repo. If your token lacks
`Zone > DNS > Edit`, `init` prints the record to add by hand and carries on;
`--skip-dns` does the same without trying.

## Use

```bash
sendup publish report.html              # -> https://<random>.example.com/
sendup publish ./site                   # directory; index.html at its root -> /
sendup publish page.html --name demo    # -> https://demo.example.com/
sendup publish page.html --ttl 30       # deletes itself after 30 days
sendup list
sendup rm <slug>
sendup rename <slug> <new-name>         # move a site to a different subdomain
sendup token <TOKEN>                    # swap the stored API token, no API call
```

A single `.html` file is uploaded as `/index.html`. A directory is uploaded verbatim,
dotfiles skipped. Extensionless paths fall back to `<path>/index.html`, and a `404.html`
at the site root is used for misses.

### Republishing

Publishing the same file or directory again **reuses the subdomain it went to last
time**, so a URL you already shared keeps working. The mapping lives in the site's
record in KV, keyed on the absolute source path, so it resolves from any machine
pointed at the same account.

- files deleted since the last publish are swept, so removed pages stop serving
- existing credentials are carried over — a republish never silently unprotects a page
- `--new` forces a fresh subdomain and repoints the source at it
- `--name NAME` pins the subdomain (`--slug` is an alias). Valid hostname label:
  lowercase letters, digits, hyphens, 2–63 chars

> **Updates take up to ~60s** to appear at an edge that already served the old version.
> Workers KV caches reads for 60 seconds and that is also the floor, so it cannot be
> tuned down. First-time publishes are immediate. This applies to access changes too —
> adding a password to a live page leaves it briefly open.

## Who can read it

Three mechanisms. They compose, and holding **any** of them gets you in.

### 1. Unguessable URL (default)

The slug is 64 bits of randomness. Nothing lists it, and every response carries
`X-Robots-Tag: noindex, nofollow, noarchive`. Anyone with the link opens it instantly,
no prompt — the same model as "anyone with the link" sharing elsewhere.

This is **unlisted, not secret**. Fine for a draft; not for anything that matters if the
URL leaks. Note that `--name` trades this away: a chosen name is guessable, so pair it
with one of the below.

### 2. Signed links — `--link`

```console
$ sendup publish proposal.html --link --link-ttl 7
https://5f2a....example.com/
  share this:
https://5f2a....example.com/?k=1754131200.9c1e4f7a2b8d6e0f3a5c7b9d1e2f4a6b
  link expires 2026-08-09 12:00 UTC
```

The page opens only for a valid signed link. No password to type — the recipient clicks
and reads. The Worker verifies the token, sets an `HttpOnly; Secure; SameSite=Lax`
cookie, and redirects to the clean URL so the token stops riding along in the address
bar and in anything they copy onward.

The token is `<expiry>.<HMAC-SHA256(secret, "slug:expiry")>`, truncated to 16 bytes. The
expiry is *inside* the signed message, so nobody can extend their own link. The secret
lives in KV metadata and never leaves your account.

```bash
sendup link <slug>                 # issue another, no expiry
sendup link <slug> --ttl 3         # issue one good for 3 days
sendup publish ... --rotate        # new secret; every link issued so far dies
```

Issuing links doesn't re-upload anything.

### 3. Password — `--auth`

```console
$ sendup publish proposal.html --auth
  basic auth  sendup / jDbhHpUaPZlbrOnH
  not recoverable later — republish to change it
```

HTTP Basic auth. `--auth user:pass` sets your own. Only a salted SHA-256 reaches KV —
the password is printed once and never stored, so changing it means republishing. The
Worker recomputes the hash and compares in constant time.

Strongest of the three: survives the URL leaking entirely. Costs the recipient a
password prompt.

### Notes

Access config lives on the site record only, never on individual files, so changing it is
a single write rather than one per file. The Worker fetches that record in parallel with
the file, which costs one extra KV read per request — halving the effective free-tier
read budget to roughly 50,000 page views a day.

The record is also what gates a miss: without it, an outsider could map which paths exist
by watching 404 versus 403. Gated responses are sent `Cache-Control: private, no-store`
and `Vary: Authorization, Cookie`.

Everything here rests on TLS, and Cloudflare terminates TLS — so Cloudflare can see your
pages in plaintext. This is access control for documents, not a secrets store. Don't
publish credentials or personal data.

## Dashboard

```bash
sendup admin --user you            # prints a generated password, once
```

Then open `https://dash.example.com/` — same Worker, no extra deploy. It lists every
site with its title, size, age and access mode, and lets you:

- **rename the subdomain** — moves every file to the new prefix; the old URL stops
  working at once, and any signed links die with it (the token is HMACed over the slug)
- **switch access** between open / signed link / password, without republishing the
  files (the config lives on the site record, so a toggle is a single write)
- **issue share links**, with or without a 7-day expiry, copied to the clipboard
- **revoke** every link issued for a site
- **delete** a site and all its files, behind a confirm
- watch storage against the 1 GB free-tier ceiling

Built with **React 19 + [Kumo](https://kumo-ui.com)** (Cloudflare's component library) and
bundled by **Bun**. The build emits one self-contained HTML document — script and styles
inlined — into `src/dashboard-html.js`, which the Worker serves directly. 193 KB gzipped,
no separate asset hosting, no runtime CDN fetch.

```bash
cd dash
bun install
bun run build     # -> ../src/dashboard-html.js
bun test          # renders the app against a stubbed API in a DOM
```

The generated file is committed, so `wrangler deploy` never needs Bun. Rebuild it after
touching anything in `dash/`.

`dash`, `www` and `api` are reserved and cannot be published as site names.

### Dashboard security

The dashboard can delete everything you have published, so it is worth knowing exactly
what stands in front of it:

- HTTP Basic auth against a salted SHA-256 in KV, compared in constant time. It is a
  separate credential from any page password.
- Browsers replay Basic auth on cross-site requests, so every mutating call also
  requires an `X-Sendup: 1` header — which a cross-origin page cannot set without a CORS
  preflight the Worker never answers — and rejects a non-same-origin `Sec-Fetch-Site`.
- Responses are `no-store`, the page is `noindex`, and a strict CSP blocks any external
  fetch.

If you would rather not have a delete endpoint on the public internet at all, don't run
`sendup admin` — every action above also exists as a CLI command, and without the
`__admin` record the dashboard returns 503 to everyone.

## Limits

Cloudflare free tier:

| Thing           | Limit                          |
| --------------- | ------------------------------ |
| Subdomains      | unbounded (one wildcard record)|
| KV storage      | 1 GB                           |
| KV writes       | 1,000/day                      |
| KV reads        | 100,000/day                    |
| Value size      | 25 MB per file                 |
| Worker requests | 100,000/day                    |

One file is one KV write, so the daily write budget is the real ceiling — roughly a
thousand files a day.

## Claude Code

`skill/SKILL.md` teaches Claude Code to publish for you — "put this on sendup", "give me
a link for this".

```bash
ln -s "$PWD/skill" ~/.claude/skills/sendup
```

## Layout

```
cli/main.go              the CLI — Go stdlib only, no dependencies
src/worker.js            the Worker — no dependencies
src/dashboard-html.js    generated by dash/build.ts, committed
dash/                    React + Kumo dashboard, bundled with Bun
test/worker.test.ts      worker tests
wrangler.template.jsonc  committed template; `sendup init` writes wrangler.jsonc
```

Two suites, both on `bun test`, no test framework beyond it:

```bash
bun run test          # both
bun run test:worker   # routing, gating, links, rename, dashboard API
bun run test:dash     # real React render against a stubbed API
bun run build         # rebuild the dashboard bundle after editing dash/
bun run deploy        # wrangler deploy
```

## License

MIT — see [LICENSE](LICENSE).
