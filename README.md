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
   | Account | Workers KV Storage  | Edit  |
   | Account | Workers Scripts     | Edit  |
   | Zone    | DNS                 | Edit  |
   | Zone    | Zone                | Read  |

   Restrict it to the `example.com` zone and your account.

2. Provision the KV namespace and the wildcard DNS record:

   ```bash
   ./bin/devdox init --token <TOKEN>
   ```

   This writes `~/.config/devdox/config.json` (mode 0600) and fills the namespace id
   into `wrangler.jsonc`.

3. Deploy the worker:

   ```bash
   CLOUDFLARE_API_TOKEN=<TOKEN> npx wrangler deploy
   ```

4. Put the CLI on `PATH`:

   ```bash
   ln -s "$PWD/bin/devdox" ~/.claude/bin/devdox
   ```

## Use

```bash
devdox publish report.html             # -> https://a3f9c1d2e4b60718.example.com/
devdox publish ./site --ttl 30         # directory, auto-deleted after 30 days
devdox publish page.html --slug demo   # -> https://demo.example.com/
devdox list
devdox rm a3f9c1d2e4b60718
```

A single `.html` file is uploaded as `/index.html`. A directory is uploaded verbatim,
dotfiles skipped. Extensionless paths fall back to `<path>/index.html`, and a
`404.html` at the site root is used for misses.

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

Pages are public to anyone holding the URL. The slug is 64 bits of randomness and
responses carry `X-Robots-Tag: noindex, nofollow, noarchive`, so pages are unlisted
rather than secret. Do not publish anything that needs authentication.
