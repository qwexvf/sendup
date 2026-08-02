---
name: sendup
description: Publish a static HTML page or a directory of static files to a random subdomain of example.com and return the URL. Use when the user asks to publish, host, deploy, or share a page, report, dashboard, mockup, or write-up on their own domain — phrasings like "put this on sendup", "publish this page", "give me a link for this", "host this HTML". Not for dynamic apps: there is no server, no database, and no build step.
---

# sendup

Publishes static files to `https://<random>.example.com/`. One Cloudflare Worker reads
the subdomain off the `Host` header and serves the matching files out of KV.

## Publish

```bash
sendup publish report.html            # single file -> becomes /
sendup publish ./site                 # directory, index.html at its root -> /
sendup publish page.html --ttl 7      # auto-deletes after 7 days
sendup publish page.html --name demo  # https://demo.example.com/ instead of random
sendup publish page.html --link       # only a signed link opens it, no password
sendup publish page.html --link-ttl 7 # signed link that expires in 7 days
sendup publish page.html --auth       # HTTP Basic auth, credentials generated
sendup publish page.html --auth me:s3cret
```

URLs are printed on stdout; everything else goes to stderr. Report URLs to the user
verbatim — never reconstruct or shorten them, especially signed links, where the `?k=`
token is the credential.

With `--auth`, the generated password prints **once** and only its salted hash is
stored. Relay it alongside the URL and say it can't be recovered — changing it means
republishing.

## Choosing a gate

- **nothing** — default. A 64-bit random subdomain, unlisted and `noindex`, but readable
  by anyone who gets the URL. Right for scratch pages and drafts.
- **`--link`** — best default when the user will *send* the page to someone. They click
  and read; no password to relay. `sendup link <slug> [--ttl N]` issues more later
  without re-uploading; `--rotate` on a publish revokes every link already issued.
- **`--auth`** — strongest; survives the URL leaking. Costs the recipient a prompt.

`--name` makes the URL guessable, so pair it with `--link` or `--auth` for anything
that isn't meant to be public.

## Republishing

Publishing the same path again reuses the subdomain it went to last time — the shared
URL keeps working, deleted files are swept, and existing auth is carried over. Use
`--new` to force a fresh subdomain, `--no-auth` to remove a gate.

A memorable `--name` is worth suggesting when the user will share or revisit the link;
random slugs are the right default for one-off drafts. A name is guessable, so pair it
with `--auth` for anything not meant for strangers.

Updates take up to ~60s to appear at an edge that already served the old version (Workers
KV read cache). Don't report a republish as live-and-verified straight after running it;
if the user needs confirmation, wait a minute before checking. First-time publishes are
immediate.

## Manage

```bash
sendup list        # every published site, newest first
sendup rm <slug>   # delete a site and all its files
```

## Rules

- **Static only.** No server-side code, no database, no SSR. Fetch calls in the page go
  straight from the browser to whatever origin they name, and CORS applies.
- **Self-contained.** No CDN discipline is enforced by the platform, but external
  requests are the usual source of broken pages — prefer inlining CSS/JS and using
  data URIs for small assets.
- **Public to anyone with the URL unless `--auth` is passed.** The slug is 16 random hex
  chars and pages carry `X-Robots-Tag: noindex`, so they will not be indexed — but that
  is obscurity, not access control. Reach for `--auth` when the page is a draft, is
  client- or business-specific, or contains anything the user would not hand to a
  stranger. Never publish credentials, tokens, or personal data at all. If in doubt, ask
  before publishing.
- **Never publish a file you have not read.** Publishing distributes the content.
- Do not publish pages impersonating a real person or organization, fabricated records
  presented as genuine, or anything that collects credentials.
- Per-file cap is 25 MB.

## When something breaks

- `no such site` — the slug was never published, or the TTL expired.
- `missing "apiToken"` — run `sendup init --token <TOKEN>`; setup is documented in
  `~/sendup/README.md`.
- Page loads but assets 404 — relative paths in a directory publish resolve against the
  site root; check the paths match the directory layout that was uploaded.
