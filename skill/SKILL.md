---
name: devdox
description: Publish a static HTML page or a directory of static files to a random subdomain of example.com and return the URL. Use when the user asks to publish, host, deploy, or share a page, report, dashboard, mockup, or write-up on their own domain — phrasings like "put this on devdox", "publish this page", "give me a link for this", "host this HTML". Not for dynamic apps: there is no server, no database, and no build step.
---

# devdox

Publishes static files to `https://<random>.example.com/`. One Cloudflare Worker reads
the subdomain off the `Host` header and serves the matching files out of KV.

## Publish

```bash
devdox publish report.html            # single file -> becomes /
devdox publish ./site                 # directory, index.html at its root -> /
devdox publish page.html --ttl 7      # auto-deletes after 7 days
devdox publish page.html --slug demo  # https://demo.example.com/ instead of random
```

The URL is printed on stdout; upload stats go to stderr. Report the URL to the user
verbatim — don't reconstruct it.

## Manage

```bash
devdox list        # every published site, newest first
devdox rm <slug>   # delete a site and all its files
```

## Rules

- **Static only.** No server-side code, no database, no SSR. Fetch calls in the page go
  straight from the browser to whatever origin they name, and CORS applies.
- **Self-contained.** No CDN discipline is enforced by the platform, but external
  requests are the usual source of broken pages — prefer inlining CSS/JS and using
  data URIs for small assets.
- **Public to anyone with the URL.** The slug is 16 random hex chars, and pages are
  served with `X-Robots-Tag: noindex`, so they will not be indexed — but there is no
  authentication. Never publish credentials, tokens, personal data, or anything the
  user described as sensitive. If in doubt, ask before publishing.
- **Never publish a file you have not read.** Publishing distributes the content.
- Do not publish pages impersonating a real person or organization, fabricated records
  presented as genuine, or anything that collects credentials.
- Per-file cap is 25 MB.

## When something breaks

- `no such site` — the slug was never published, or the TTL expired.
- `missing "apiToken"` — run `devdox init --token <TOKEN>`; setup is documented in
  `~/devdox/README.md`.
- Page loads but assets 404 — relative paths in a directory publish resolve against the
  site root; check the paths match the directory layout that was uploaded.
