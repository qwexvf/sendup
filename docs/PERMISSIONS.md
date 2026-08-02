# Permissions

Everything sendup does runs against one Cloudflare account. This page says exactly which
permissions are needed, which command needs them, and how to hand out less than all of
them.

## The short version

Create a token at <https://dash.cloudflare.com/profile/api-tokens> → **Create Token** →
**Custom token**:

| Scope   | Permission         | Level | Needed by                       | Optional?          |
| ------- | ------------------ | ----- | ------------------------------- | ------------------ |
| Account | Workers KV Storage | Edit  | `init`, `publish`, `list`, `rm`, `link`, `admin` | no |
| Account | Workers Scripts    | Edit  | `wrangler deploy`               | after first deploy |
| Zone    | Workers Routes     | Edit  | `wrangler deploy`               | after first deploy |
| Zone    | Zone               | Read  | `init`                          | after `init`       |
| Zone    | DNS                | Edit  | `init` (wildcard record)        | yes, see below     |

**Zone Resources:** Include → Specific zone → your domain.
**Account Resources:** Include → your account.

## It must be a user-owned token

Tokens come in two kinds and only one of them works:

- **User tokens** — <https://dash.cloudflare.com/profile/api-tokens>. Can hold Account
  *and* Zone permissions. **Use these.**
- **Account-owned tokens** — Manage Account → API Tokens. Cannot hold zone-scoped
  permissions at all.

If the permission editor shows no **Zone** section in the scope dropdown, you are on the
account-owned page. Delete the token and make a user one; there is no way to add zone
permissions to an account-owned token afterwards.

## What each command actually calls

Useful when a call fails and you want to know which checkbox is missing. sendup names
the missing permission in the error, but this is the full map.

| Command | Cloudflare endpoint | Permission |
| ------- | ------------------- | ---------- |
| `init` | `GET /zones?name=` | Zone > Zone > Read |
| `init` | `GET,POST /accounts/:a/storage/kv/namespaces` | Account > Workers KV Storage > Edit |
| `init` | `GET,POST /zones/:z/dns_records` | Zone > DNS > Edit |
| `publish` | `PUT /accounts/:a/storage/kv/namespaces/:n/bulk` | Account > Workers KV Storage > Edit |
| `publish` | `GET .../keys` (reuse the subdomain, sweep stale files) | Account > Workers KV Storage > Edit |
| `publish` | `POST .../bulk/delete` (sweep) | Account > Workers KV Storage > Edit |
| `list`, `link` | `GET .../keys` | Account > Workers KV Storage > Edit |
| `rm` | `GET .../keys`, `POST .../bulk/delete` | Account > Workers KV Storage > Edit |
| `admin` | `PUT .../bulk` | Account > Workers KV Storage > Edit |
| `wrangler deploy` | upload script | Account > Workers Scripts > Edit |
| `wrangler deploy` | bind `*.domain/*` | Zone > Workers Routes > Edit |

Workers KV **Read** is not enough for anything except `list`, because `publish` writes
and `rm` deletes. There is no read-only mode worth configuring.

## Handing out less

### Skip DNS entirely

`Zone > DNS > Edit` exists so `init` can create one record, once. If you would rather not
grant write access to your DNS:

```bash
sendup init --token <TOKEN> --skip-dns
```

Then add the record by hand in the dashboard:

| Type | Name | IPv6 address | Proxy status |
| ---- | ---- | ------------ | ------------ |
| AAAA | `*`  | `100::`      | **Proxied**  |

`100::` is the IPv6 discard prefix. Nothing is ever sent there — the record exists only
so Cloudflare's proxy accepts the hostname and hands the request to the Worker. Without
`--skip-dns`, sendup tries, and on a 403 prints these instructions and carries on.

### A publish-only token

After `init` has run once, the config at `~/.config/sendup/config.json` holds the account,
zone and namespace ids, so nothing looks them up again. Day-to-day use needs exactly one
permission:

| Scope   | Permission         | Level |
| ------- | ------------------ | ----- |
| Account | Workers KV Storage | Edit  |

So a reasonable split is: run `init` and `wrangler deploy` with a full token, delete it,
then swap in a KV-only token for daily use:

```bash
sendup init --token <SETUP_TOKEN>
CLOUDFLARE_API_TOKEN=<SETUP_TOKEN> npx wrangler deploy
# delete the setup token in the dashboard, then:
sendup token <KV_ONLY_TOKEN>
```

`sendup token` only rewrites the stored value — it makes no API call, so a narrow token
cannot fail the swap. Re-running `init` would *not* work here: `init` starts with a zone
lookup that a KV-only token has no permission for.

You will need a full token again to redeploy the Worker after changing `src/`. Pass it
inline rather than storing it:

```bash
CLOUDFLARE_API_TOKEN=<SETUP_TOKEN> npx wrangler deploy
```

That KV-only token can still delete every page you have published — Workers KV Edit is
namespace-wide. It cannot touch DNS, deploy code, or reach any other part of the account.

## Where the token lives

`~/.config/sendup/config.json`, created mode `0600` in a directory created mode `0700`:

```json
{
  "apiToken": "…",
  "accountId": "…",
  "zoneId": "…",
  "namespaceId": "…",
  "domain": "example.com"
}
```

`CLOUDFLARE_API_TOKEN` in the environment overrides the stored value, which is the way to
use a different token for one command without rewriting the file. `SENDUP_DOMAIN`
overrides the domain the same way.

The token is never sent anywhere except `api.cloudflare.com`, and never embedded in a
published page or in the Worker.

## The Worker's own permissions

The deployed Worker does **not** hold an API token. It reaches KV through the `SITES`
binding declared in `wrangler.jsonc`, which Cloudflare scopes to that one namespace.

This matters for the dashboard: its ability to delete sites comes from that binding, not
from a credential anyone could steal off the page. The only thing standing in front of it
is the password set by `sendup admin`. See the Dashboard security section in the README.

If you never run `sendup admin`, no `__admin` record exists, and `dash.<domain>` returns
503 to everyone — including you. Every dashboard action also exists as a CLI command, so
declining to set it up costs nothing but convenience.

## Rotating and revoking

| To do this | Do |
| ---------- | -- |
| Rotate the API token | Roll it in the Cloudflare dashboard, then `sendup token <NEW>` |
| Change the dashboard password | `sendup admin --password <NEW>` — takes effect within ~60s |
| Turn the dashboard off | Delete the `__admin` key from the KV namespace in the Cloudflare dashboard |
| Kill every share link for a site | `sendup publish <path> --rotate`, or **revoke** in the dashboard |
| Change a page password | `sendup publish <path> --auth user:newpass` |
| Take a page down | `sendup rm <slug>` |

Access changes are subject to the same ~60 second Workers KV read cache as content
updates. A revoked link can still work for up to a minute at an edge that already served
it. If you need something gone *now*, `sendup rm` deletes the files, and a missing file
cannot be served from cache.
