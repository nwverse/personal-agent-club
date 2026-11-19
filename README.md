# personal-agent-club

Personal Agent Club ("club") keeps your personal agents in one place: accounts you pair at agent services, their
sessions and messages, an outbox for what you send them, read state and search. It is one Go program that runs in
either of two ways:

- **As a Persona extension.** A Persona hub starts the program with no arguments and talks to it over the extension
  protocol on stdin and stdout. It contributes the agent source `pac`, so its sessions show up on Persona's Agents
  page (session ids `pac:<sessionKey>`).
- **Standalone, with `serve`.** The program serves the same protocol over HTTP on a unix socket (and, with tokens, on
  TCP), for the Lark gateway's agent bridge or any other client.

Both shells call the same core and answer the same protocol.

## Status

**Usable with the API providers.** `serve` keeps each space in SQLite with its credentials encrypted on disk, and
reaches three providers through their APIs: Cue (Manus API v2), Grok Bot (its desktop client's protocol) and Groq
(OpenAI-compatible). An exported space of the Lark gateway's agent bridge can be imported with its ids, and the
accounts can be listed read-only beside a running serve. The browser-based providers (Muse, Dots, Claude Duet) are not
in this version. The provider adapters are built from documentation and recorded fixtures and still need calibrating
against real traffic (`internal/provider/testdata`). The Persona extension serves the same core from its own data
directory, with an accounts page and the assistant's tools. See the roadmap below.

License: not decided yet. Until a license is added, no rights beyond viewing the source are granted.

## Running

```sh
make build                     # bin/personal-agent-club
bin/personal-agent-club version
```

### The data directory

`serve` and `import` keep everything in one data directory (`-data`, default `~/.personal-agent-club`), created with
mode 0700:

| Path | What |
| --- | --- |
| `key` | the key of the credential files (64 hex digits, 0600; created on first start; `-key-file` puts it elsewhere) |
| `club.lock` | held by the one process that owns the directory: a serve, an import, or the Persona extension |
| `club.sock` | serve's default unix socket |
| `spaces/<space>/club.db` | the space's SQLite store (WAL): the gateway's `agent_accounts`, `agent_sessions`, `agent_messages` and `agent_outbox` tables, plus settings |
| `spaces/<space>/credentials.enc` | the space's credentials, AES-256-GCM under the key, bound to the space's name |

A second owner is refused (`in use by another personal-agent-club process`). A key file that does not open a space's
credentials is refused when the space opens, before serve listens, and nothing is overwritten. Keep the key out of
backups of the directory, or the encryption protects nothing. Quota readings are kept in memory only.

### serve

```sh
bin/personal-agent-club serve                                  # ~/.personal-agent-club, socket ~/.personal-agent-club/club.sock
bin/personal-agent-club serve -data /srv/club -poll 2m
bin/personal-agent-club serve -listen 127.0.0.1:7655 -tokens tokens.json
```

| Flag | |
| --- | --- |
| `-data DIR` | the data directory |
| `-key-file F` | the key file (default `<data>/key`) |
| `-socket PATH` | the unix socket (default `<data>/club.sock`; `none` for TCP only) |
| `-listen ADDR` | a TCP address as well; needs `-tokens` |
| `-tokens F` | the tokens file |
| `-poll D` | how often every account is synced (default `60s`; `0` turns polling off; at least `15s`) |
| `-demo-echo` | add the in-memory echo provider (demos and tests only) |

At start serve opens every space already in the directory: their outboxes recover (sends that were in flight become
`unconfirmed`, sends still pending go out once) and their polling starts. Without polling, accounts are synced when
paired, on `club.accounts.sync` and after sends.

By default `serve` listens only on the unix socket, created with mode 0600; whoever can open the socket is trusted. To
listen on TCP as well, give `-listen` and a tokens file; without tokens, `serve` refuses to start:

```json
{"read": ["<read token>"], "write": ["<write token>"], "admin": ["<admin token>"]}
```

Tokens are at least 32 characters and sent as `Authorization: Bearer <token>`. Levels:

| Level | May call |
| --- | --- |
| `read` | `agentsrc.status`, `sessions`, `messages`, `search`, the event stream |
| `write` | also `agentsrc.send`, `delete_message`, `mark_read` |
| `admin` | also every `club.*` method |

`mark_read` needs `write` because it changes unread state, as with the Lark gateway's read-only agent token.

Try it with curl:

```sh
S=~/.personal-agent-club/club.sock
curl -s --unix-socket $S http://club/v1/spaces/default/rpc -d '{"method":"club.info"}'
curl -s --unix-socket $S http://club/v1/spaces/default/rpc \
  -d '{"method":"club.accounts.add","params":{"provider":"groq","credential":"<your Groq API key>"}}'
curl -s --unix-socket $S http://club/v1/spaces/default/rpc -d '{"method":"agentsrc.sessions"}'
curl -sN --unix-socket $S http://club/v1/spaces/default/events
```

### import

```sh
bin/personal-agent-club import [-data DIR] [-key-file F] [-space default] [-only-provider NAME]... [-dry-run] [-json] BUNDLE.json
```

Imports a space exported from the Lark gateway (format: [docs/import-bundle.md](docs/import-bundle.md)), keeping every
id, skipping the accounts of providers this build cannot reach, never sending anything, and then verifying each
imported account read-only. Stop serve first; `-dry-run` writes nothing (not even the directory or the key) and works
beside a running serve. Exit codes: 0 done, 1 error, 2 usage, 3 imported but an imported account did not verify.

### accounts

```sh
bin/personal-agent-club accounts [-data DIR] [-space default] [-json]
```

Lists a space's accounts (id, provider, name, status, last synced, last error, sessions, messages, assistant control,
quota display) straight from the store, read-only: no lock, nothing created, the credentials never opened. It works
while serve runs.

### Environment

| Variable | |
| --- | --- |
| `PERSONAL_AGENT_CLUB_PROXY` | every provider request goes through this proxy (`socks5://`, `socks5h://`, `http://` or `https://` with host and port), and fails rather than going direct when it is down; unset, `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` apply |
| `PERSONAL_AGENT_CLUB_GROQ_MODEL` | the model Groq accounts ask (default `openai/gpt-oss-120b`); a pairing is refused when the key cannot use it |

Provider requests never follow a redirect, so a credential header never goes to another host.

### As a Persona extension

`persona-extension.json` is the manifest: extension id `personal-agent-club`, the agent source `pac` (providers Cue,
Grok Bot, and Groq), the Accounts page (`ui` `accounts`) and the connector `club` with the assistant's `club_agent_*`
tools, `activation_events: on_startup`, and network access to the three providers' API hosts only. `make package`
builds `dist/personal-agent-club-<version>.tar.gz` (manifest and the four programs), `scripts/persona-test.sh` holds
it to the hub's own checks, and [docs/persona-install.md](docs/persona-install.md) installs it on a hub. The
configuration switch `demo_echo` adds the echo provider. The extension syncs every account every 60 s and keeps its
one space (`default`) in the data directory the hub gives it, laid out as serve's with the key in `club.key`, so
`import` and `accounts` work on it too; its providers go out through the HTTP client that takes the hub's egress
proxy. Without a data directory (run by hand) it keeps everything in memory.

## Providers

| | Cue | Grok Bot | Groq |
| --- | --- | --- | --- |
| Name | `cue` | `grok_bot` | `groq` |
| Connects with | API key (`x-manus-api-key`) | device sign-in (`club.logins.grok_bot.*`, approved in the user's browser) or a pasted access token | API key (`Authorization: Bearer`) |
| History | from the provider: each agent's main task and its subtasks | from the provider: each bot's main session and side sessions | local only: Groq keeps no conversation; club stores it and sends the latest 20 turns (24k characters) with each message |
| Send | yes, into a main task; subtasks are read-only | yes; the client id is the message id, so a repeat is a duplicate, not a second message | yes; the answer comes back in the same call |
| Send confirmation (of an `unconfirmed` send) | no: it stays unconfirmed | yes: the send status by message id, asked at the follow-ups | no: it stays unconfirmed |
| Live state (`club.live`) | no (`supported: false`) | yes: running and composing, per session | no (`supported: false`) |
| Push | no: polling | after a send, the transcript watch (at most 90 s) reads the session as soon as it changes | not needed |
| Interrupt | no | no | no |
| Start a session | no | no | no |
| Credential refresh | no | yes, for signed-in credentials (refresh token), once per refused token; a pasted token cannot be refreshed | no |
| Quota | a 429 (with `Retry-After`) | a 429 (with `Retry-After`) | every answer's `x-ratelimit-*` headers (requests per day, tokens per minute; the model list's too), and a 429 |
| Hosts | `api.manus.ai` | `api2.cursor.sh` (the sign-in page the user opens is on `cursor.com`) | `api.groq.com` |

`muse`, `dots` and `claude_duet` (browser-based) are not in this version: listed as unavailable, refused when paired
(`unsupported`, `PROVIDER_UNAVAILABLE`), skipped by an import. `club.sessions.start` and `club.sessions.interrupt`
answer `unsupported` for every provider.

## Protocol

Calls are `{method, params}`; results and errors follow Persona's agent source protocol field for field (results in
camelCase, parameters in snake_case), so the extension shell passes them through unchanged. Over HTTP:
`POST /v1/spaces/{space}/rpc` with `{"method", "params"}` → `{"result"}` or `{"error": {"code", "message", "reason"?}}`.

| Method | Params | Result |
| --- | --- | --- |
| `agentsrc.status` | — | `{enabled, setup?, providers, accounts}` |
| `agentsrc.sessions` | — | `{sessions: [...]}` |
| `agentsrc.messages` | `session_key`, `cursor?`, `limit?` | `{session?, messages, hasMore, nextCursor?}` |
| `agentsrc.send` | `session_key`, `client_id` (UUID), `text` | the message as queued, with `local {clientId, status, error?}` |
| `agentsrc.delete_message` | `session_key`, `client_id` | `{}`: cancels a send that has not gone out |
| `agentsrc.mark_read` | `session_key` | `{}` |
| `agentsrc.search` | `q` | `{hits: [...]}` (words, and CJK by n-grams) |
| `club.info` | — | `{version, protocolVersion, providers, browser}` |
| `club.accounts.add` | `provider`, `credential`, `display_name?` | the account (verified with the provider, then synced once) |
| `club.accounts.update` | `account_id`, `paused?`, `display_name?`, `assistant_control?`, `show_quota?` | the account |
| `club.accounts.replace_credential` | `account_id`, `credential` | the account (the same remote account only) |
| `club.accounts.remove` | `account_id`, `delete_data?` | `{}` |
| `club.accounts.sync` | `account_id` | the account after one sync round |
| `club.live` | `session_key` | `{supported, running, composing}` (asked at most every 3 s per session) |
| `club.settings.get` / `set` | — / `browser_identity` | `{browserIdentity}` (kept only, in this version) |
| `club.outbox.dismiss` | `session_key`, `client_id` | `{}`: removes a failed send, releasing its client id |
| `club.logins.grok_bot.start` | `account_id?` (sign that account in again) | `{loginId, loginUrl}` (10 minutes; at most two waiting) |
| `club.logins.grok_bot.poll` | `login_id`, `display_name?` | `{status: pending}` or `{status: done, account}` |
| `club.logins.grok_bot.cancel` | `login_id` | `{}` |
| `club.grok_bot.takeover.start` | `account_id` | `{takeoverId, phase, detail}`: begin linking that Grok Bot's own machine |
| `club.grok_bot.takeover.status` | `takeover_id` | `{phase, detail, node, paired, ssh, waiting}`: ask the bot for the next step and where it stands |
| `club.grok_bot.takeover.plan` | `takeover_id` | `{planId, summary, node, kind, host, alreadyLinked}`: the hub change, shown and not yet written |
| `club.grok_bot.takeover.apply` | `takeover_id` | `{applied, detail, backup?}`: write the machine list, only after `plan`; the hub is not restarted |
| `club.import` | `path`, `only_providers?`, `dry_run?` | `{accounts, skipped, dryRun?}` ([docs/import-bundle.md](docs/import-bundle.md)) |
| `club.sessions.start`, `club.sessions.interrupt` | | `unsupported` (`PROVIDER_UNAVAILABLE`): no provider in this build can |
| `club.export`, `club.logins.browser.*` | | `unsupported`: not in this version |

Error codes: `not_found`, `invalid`, `conflict`, `unsupported`; other errors carry only a message. Over HTTP they come
with 404, 400, 409, 501 (502 for the rest). `reason`, when present, is the Lark gateway's agent bridge error code
(`AGENT_SESSION_NOT_FOUND`, `ACCOUNT_PAUSED`, `LOGIN_LIMIT`, …) or a provider's (`AUTH_FAILED`, `RATE_LIMITED`, …).

Event `agentsrc.updated {session_keys?}`: those sessions changed; no `session_keys` means the list itself changed. Over
HTTP: `GET /v1/spaces/{space}/events`, server-sent events.

Additions Persona hubs ignore: sessions carry `previewText`, `previewIsMine` and `onTopRankTimeMs`; a message's
`local` keeps `error`; accounts and sessions carry `quota` when something is known.

Rules the core keeps:

- **Spaces** isolate accounts, data and credentials (`/v1/spaces/{space}`); the Persona shell uses `default`.
- **Ids**: account ids are `a` and 12 hex digits; session keys `agent:<provider>:<accountId>:<externalId>`.
- **Outbox**: sends are deduplicated by `client_id`. A send whose outcome is unknown is marked `unconfirmed` and is
  never sent again automatically (a provider that can confirm is asked, read-only); after a restart, sends that were
  in flight become `unconfirmed` too, and sends still pending go out once.
- **Sync** follows the gateway's schedule: at most 20 sessions per round (those that moved, those active in the last
  30 minutes, then the three read least recently); a refused credential is retried every 15 minutes, after one
  refresh where the provider has one.
- **Credentials** go in and never come out: no method returns one, nothing logs one.
- **Quota** comes only from what costs nothing: the readings a provider's answers carry and any 429. `state` is `ok`
  (more than 20% left), `low` (20% or less) or `exhausted` (none left, or a 429, until its `Retry-After` or else for
  five minutes); the tightest dimension is on top, the others in `details`; `stale` after 15 minutes without an
  answer; `show` mirrors the account's `show_quota`. Unknown is no `quota` at all. A change of state or staleness is
  announced with `agentsrc.updated`. Sends to an exhausted account are still tried.
- **Assistant control** per account (`off`, `read`, `confirm`, `auto`; `off` by default, and for every imported
  account): what the Persona assistant may do with that account's agents. With nobody present (scheduled tasks,
  background follow-ups) the assistant may only read, whatever the level.
- **Linking a Grok Bot's machine** (`club.grok_bot.takeover.*`, the 夷舍 button on a Grok Bot account): the bot is asked,
  through its own session, to prepare its own machine as a Persona node (the node and its supervisor, the tailcat
  pairing, the dedicated sshd). The hub's machine list is only described until `club.grok_bot.takeover.apply`, which
  writes it after `plan` and never restarts the hub. A node already in the list is reported and left as it is. Nothing
  in these calls carries a key, a token or an address.

## Layout

| Path | What |
| --- | --- |
| `cmd/personal-agent-club` | the program: extension mode, `serve`, `import`, `accounts`, `version`; the whole-program tests |
| `scripts` | `package.sh` (the Persona package, `make package`) and `persona-test.sh` (the hub's own checks) |
| `internal/protocol` | wire types and method names, pinned by golden JSON |
| `internal/provider` | the provider adapter interface, optional capabilities, the registry; `cue/`, `grokbot/`, `groq/` the API providers, `echo/` the demo one, `web/` their shared HTTP client, `providertest/` a fake upstream |
| `internal/providers` | the registry both shells build: the API providers on one HTTP client and its proxy |
| `internal/store` | the store interface, SQLite (with CJK search) and in-memory implementations, one test suite for both |
| `internal/vault` | credentials: the encrypted file and an in-memory one |
| `internal/datadir` | the data directory: lock, key, spaces, read-only access |
| `internal/core` | spaces, accounts, sync, outbox, read state, search, events, quota, import, logins, assistant control |
| `internal/serve` | the HTTP shell and its token levels |
| `internal/extension` | the Persona extension shell: the agent source, the accounts page, the assistant's `club_agent_*` tools, its data directory |
| `internal/conformance` | one protocol test suite, run against both shells |

## Development

```sh
make check    # gofmt, go vet, go test -race
make dist     # the four platform binaries
make package # the Persona extension package under dist/
scripts/persona-test.sh   # the hub's own checks (needs personad)
```

## Roadmap

1. The Lark gateway draws an internal boundary around its built-in agent bridge (there; no behavior change).
2. **This repository**: core and `serve` shell, protocol conformance tests, SQLite storage, the encrypted vault, the API
   providers with their recorded fixtures, quota from answers, assistant control in accounts, import. *(Done; the
   providers still need calibrating against real traffic.)* Interrupt and new sessions wait for a provider that can.
3. The Lark gateway gains a club backend behind an optional setting, off by default.
4. Migration of paired accounts into club (`import`, with verification), then rollback and export.
5. The Persona extension: agent source, accounts page and the assistant's `club_agent_*` tools on disk *(done)*, then
   gateway and launcher quota sources.
6. Browser-based providers, after checking that a browser can run in the extension sandbox.
7. Whether club becomes the Lark gateway's default backend is decided later.
