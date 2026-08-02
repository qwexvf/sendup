# devdox

Publish static HTML to `https://<random>.example.com/`.

One Cloudflare Worker on a `*.example.com` route reads the subdomain off the `Host`
header and serves the matching files out of Workers KV. Subdomains cost nothing — the
zone holds a single wildcard DNS record no matter how many sites exist, so the 200
records/zone free-plan cap never comes into play.

```
*.example.com  AAAA 100:: (proxied)
      |
      +-- worker route *.example.com/*
                |
                +-- KV  "<slug>:/index.html"  ->  bytes
```

## Setup

1. Create an API token at <https://dash.cloudflare.com/profile/api-tokens> with:

   | Scope   | Permission          | Level |
   | ------- | ------------------- | ----- |
   | Scope   | Permission          | Level | Needed for                          |
   | ------- | ------------------- | ----- | ----------------------------------- |
   | Account | Workers KV Storage  | Edit  | `init`, `publish`, `rm`             |
   | Account | Workers Scripts     | Edit  | `wrangler deploy`                   |
   | Zone    | Workers Routes      | Edit  | `wrangler deploy` — binds the route |
   | Zone    | Zone                | Read  | `init` (zone + account lookup)      |
   | Zone    | DNS                 | Edit  | wildcard record — optional          |

   Restrict it to the `example.com` zone and your account. It must be a **user-owned**
   token (`profile/api-tokens`) — account-owned tokens cannot hold zone permissions.

2. Build the CLI and put it on `PATH`:

   ```bash
   (cd cli && go build -o devdox .)
   ln -s "$PWD/cli/devdox" ~/.claude/bin/devdox
   ```

3. Provision the KV namespace and the wildcard DNS record:

   ```bash
   devdox init --token <TOKEN>
   ```

   Writes `~/.config/devdox/config.json` (mode 0600) and fills the namespace id into
   `wrangler.jsonc`. If the token has no `Zone > DNS > Edit`, it prints the record to
   create by hand and carries on — `--skip-dns` does the same without trying.

4. Deploy the worker:

   ```bash
   CLOUDFLARE_API_TOKEN=<TOKEN> npx wrangler deploy
   ```

## Use

```bash
devdox publish report.html             # -> https://a3f9c1d2e4b60718.example.com/
devdox publish ./site --ttl 30         # directory, auto-deleted after 30 days
devdox publish page.html --name demo   # -> https://demo.example.com/
devdox publish page.html --auth        # basic auth, credentials generated
devdox publish page.html --auth me:s3cret
devdox list
devdox rm a3f9c1d2e4b60718
```

A single `.html` file is uploaded as `/index.html`. A directory is uploaded verbatim,
dotfiles skipped. Extensionless paths fall back to `<path>/index.html`, and a
`404.html` at the site root is used for misses.

### Republishing

Publishing the same file or directory again reuses the subdomain it went to last time,
so a URL you have already shared keeps working. The mapping lives in the site's
`__index:` record in KV — keyed on the absolute source path — rather than in local
state, so it survives from any machine pointed at the same account.

- files deleted since the last publish are swept, so removed pages stop serving
- existing Basic auth is carried over; `--no-auth` removes it
- `--new` forces a fresh subdomain and repoints the source at it, orphaning the old site
- `--name NAME` overrides the lookup entirely and pins the subdomain
  (`--slug` is accepted as an alias). Must be a valid hostname label: lowercase
  letters, digits and hyphens, 2–63 chars.

**A republish takes up to ~60 seconds to appear at an edge that already served the old
version.** Workers KV caches reads for 60s and that is also the minimum, so it cannot be
tuned down. A first-time publish is visible immediately; only updates are affected. This
applies to auth changes too — adding `--auth` to a live site leaves it briefly open.

## Claude Code

`skill/SKILL.md` is the skill definition. Install it with:

```bash
ln -s "$PWD/skill" ~/.claude/skills/devdox
```

## Limits

Free plan, per Cloudflare's published limits:

| Thing                 | Limit                        |
| --------------------- | ---------------------------- |
| Subdomains            | unbounded (1 wildcard record)|
| KV storage            | 1 GB                         |
| KV writes             | 1,000/day                    |
| KV reads              | 100,000/day                  |
| Value size            | 25 MB                        |
| Worker requests       | 100,000/day                  |

Each published file is one KV write, so the daily write budget is the real ceiling —
roughly a thousand files a day.

## Security

By default a page is readable by anyone holding the URL. The slug is 64 bits of
randomness and responses carry `X-Robots-Tag: noindex, nofollow, noarchive`, so pages
are **unlisted rather than secret**.

`--auth` adds HTTP Basic auth. The CLI generates a 16-byte salt and stores only
`sha256(salt + "user:pass")` in KV metadata — the password itself is printed once at
publish time and never persisted, so changing it means republishing. The worker
recomputes the hash from the `Authorization` header and compares it with a constant-time
loop.

The hash rides on every object's metadata, so an authenticated hit costs no extra KV
read. It's also on the site's `__index:` record, which the worker consults on a miss —
without that, an unauthenticated caller could probe which paths exist by watching 404
versus 401. Authenticated responses are sent `Cache-Control: private, no-store` and
`Vary: Authorization`.

Basic auth sends credentials base64-encoded on every request, so it relies entirely on
TLS. Cloudflare terminates HTTPS here, which means Cloudflare can see the plaintext.
It's a reasonable gate for "don't show this to strangers"; it is not a secrets store.
