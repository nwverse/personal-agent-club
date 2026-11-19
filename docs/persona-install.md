# Installing the Persona extension from a local package

This guide installs Personal Agent Club on a Persona hub from a package you build yourself, before it is published
in a market. Everything below runs on the hub's machine, as the user the hub runs as.

## What the package is

`make package` writes `dist/personal-agent-club-<version>.tar.gz` and its `.sha256`. The archive holds one directory,
`personal-agent-club/`, with:

- `persona-extension.json`, and the same manifest as `persona-plugin.json` for hubs from before extensions were
  renamed from plugins;
- `bin/personal-agent-club-<platform>` for `linux-amd64`, `linux-arm64`, `darwin-arm64` and `darwin-amd64`.

There is no source in it. The programs are built with `-trimpath` and without cgo; the packaging script refuses to
pack when a program contains a path of the machine that built it, and the archive's entries carry no owner name or
macOS metadata. The hub needs no Go toolchain or other runtime.

Before installing, check the archive against its hash:

```sh
shasum -a 256 -c personal-agent-club-<version>.tar.gz.sha256   # or: sha256sum -c …
```

To hold the program to the hub's own checks without a hub (manifest and package as at install, handshake, error
codes, every page drawable), run `scripts/persona-test.sh` with a `personad` on `PATH` (or `PERSONAD=/path/to/personad`).

## Back up first

An install or upgrade touches two directories under the hub's state directory (`~/.persona` unless `PERSONA_HOME`
says otherwise):

| Directory | What it holds |
| --- | --- |
| `extensions/installed/personal-agent-club/<version>/` | The unpacked package; an upgrade keeps the current and the previous version |
| `extensions/data/personal-agent-club/` | The extension's own data (below): `club.key`, `club.lock` and `spaces/default/` with `club.db` and `credentials.enc` |

Copy both before an upgrade, keeping modes, for example from the state directory:

```sh
tar -C ~/.persona/extensions -cpzf personal-agent-club-backup.tgz \
  installed/personal-agent-club data/personal-agent-club
```

Treat the copy of the data directory like a
password store, since it holds the key next to what it protects. The hub also keeps the extension's settings and its
record in its database; back that up as you usually back up the hub.

Accounts, sessions, messages, read state and the outbox are kept on disk in the data directory and survive restarts,
upgrades and settings changes; the credentials are kept there encrypted. Quota readings are kept in memory only: after
a restart nothing is shown until a provider's next answer.

## Install

```sh
personad extension install file:/path/to/personal-agent-club-<version>.tar.gz
```

The command first shows what the extension contributes and asks for, then asks to continue (`--yes` skips the
question). It contributes:

- the agent source `pac` (its sessions appear on the Agents page as `pac:<sessionKey>`);
- the page **Settings › Extensions › Personal Agent Club › Accounts**;
- the connector `club`, which carries the assistant's `club_agent_*` tools;

and asks for network access to `api.manus.ai` (Cue), `api2.cursor.sh` (Grok Bot) and `api.groq.com` (Groq).

**No hub restart is needed.** The hub starts the extension right after the install (it runs from startup on, as
agent sources must) and the source appears on the Agents page within seconds. `personad extension list` should show
it as `running`. Changing a setting restarts only the extension, not the hub.

To try it without any account, switch on the demo provider, which answers every message with its own text and
reaches nothing:

```sh
personad extension config personal-agent-club demo_echo=true
```

Then pair an "Echo (demo)" account on the Accounts page with any text starting with `echo-`.

## Accounts page

**Settings › Extensions › Personal Agent Club › Accounts** lists the paired accounts with their provider, status, last
sync and last error, and pairs new ones:

- **Cue** and **Groq**: paste an API key. The key field is a secret field: the apps never show it, the extension
  cannot fill it in, and the extension never shows, returns or logs a saved key.
- **Grok Bot**: start the sign-in, approve on the page that opens in your browser, and the page checks the result every
  30 seconds (or at once with *check now*). On a paired Grok Bot account, **夷舍** (Link its machine) asks that bot to
  prepare its own machine as a Persona node. Nothing is written on the hub until you press *Confirm and write the
  machine list*, which asks again first; the hub is not restarted. A machine already linked is reported and left as it
  is.

An account's page syncs it now, pauses or resumes its syncing, unpairs it (keeping or deleting its sessions and
messages) and sets its **assistant control**. When the club behind the page cannot do something yet, the page says
so ("Not supported: …") instead of pretending.

## The assistant's tools

The connector `club` (Settings › Connectors) gives the assistant `club_agents_list`, `club_agent_read`,
`club_agent_send`, `club_agent_start` and `club_agent_interrupt`:

- An account the assistant may use is chosen on its page under **Assistant control**: *Off* (the default for every
  account: the assistant does not see it), *Read* (list and read) or *Confirm* (also write).
- Every write is confirmed on your phone, every time, whatever your gate rules say: the manifest lists the writes in
  `confirm_always`. A scheduled task or background follow-up, with nobody there to confirm, can only read.
- Starting a new session and interrupting a turn depend on the provider; where they are not available the tool says
  so before anything is asked.

## Sandbox and network

A package installed from a file runs **in the hub's sandbox** by default: it reads and writes only its own package and
data directory, and reaches the network only through the hub's egress proxy, which lets through only the hosts in
`permissions.network`. The hub hands the proxy to the program in `HTTPS_PROXY` / `HTTP_PROXY`, and the program's HTTP
client takes its proxy from there. Check the counters after the first sync:

```sh
personad extension show personal-agent-club   # "sandbox": {"active": true, "egress": {"allowed": …, "denied": …}}
```

- `denied` growing with `last_denied` naming a host means a provider reached somewhere the manifest does not list:
  report it rather than widening anything by hand.
- Keep the sandbox on. Turning it off (`personad extension sandbox personal-agent-club off`) lets the program read and
  write whatever the hub's user can, and the network list is then only shown, not enforced.
- On Linux the sandbox needs `bwrap` (bubblewrap) and, for memory and CPU limits, user systemd. Without `bwrap` the
  extension still runs, pointed at the proxy but not held to it, and its details say "No sandbox".
- If the hub itself must go through a proxy to reach the internet, set `HTTPS_PROXY` for the hub; the egress proxy
  goes out through it and the extension needs nothing more.

## Stopping just this extension

```sh
personad extension disable personal-agent-club   # its program stops; the hub and other extensions keep running
personad extension enable personal-agent-club    # started again with the same settings and data
```

`disable` keeps the settings and the data directory; the `pac` source leaves the Agents page until `enable`. Work
on the data directory while the program is stopped (an offline import, a restore) goes between the two. A hub
restart starts the extension again by itself.

## The data directory

The hub gives the program `PERSONA_EXTENSION_DATA`, which is `<state>/extensions/data/personal-agent-club/`, where
`<state>` is the hub's state directory (`~/.persona` of the user the hub runs as, or `PERSONA_HOME`). The program keeps
there:

| Path | What |
| --- | --- |
| `club.key` | the key the credential files are sealed with (AES-256-GCM), 0600; created at the first start and never rewritten |
| `club.lock` | held while the program runs: one process owns the directory at a time |
| `spaces/default/club.db` | the store of the one space this shell serves: SQLite (with its `-wal` and `-shm` files while it runs) |
| `spaces/default/credentials.enc` | that space's credentials, sealed with `club.key` |

This is the data directory layout `personal-agent-club serve` uses, with the key under the name `club.key`, so the
command's tools work on it while the extension is disabled: `personal-agent-club import -data <dir> -key-file
<dir>/club.key BUNDLE.json` imports a space exported from the Lark gateway ([import-bundle.md](import-bundle.md)), and
`personal-agent-club accounts -data <dir>` lists the accounts read-only (that one also works while the extension
runs). An import while the extension runs is refused (the lock), as is a second copy of the program. A key file that
does not open `credentials.enc` stops the program at start, and nothing is overwritten: keep `club.key` with the rest.

To see the path a running program actually has, on Linux:

```sh
pid=$(pgrep -f 'extensions/installed/personal-agent-club/.*/bin/personal-agent-club-' | head -1)
tr '\0' '\n' </proc/$pid/environ | grep '^PERSONA_EXTENSION_DATA='
```

Print only that line: the same environment holds the program's proxy credential.

Files put there by anything else (a restore, an import tool) must be owned by the user the hub runs as and be
readable and writable by that user; `0600` files in `0700` directories are what the extension writes itself. The
sandbox lets the program read and write everything under its data directory and nothing above it, and it does not
change owners or modes: a file left owned by root, or another user, cannot be opened by the program. Run such tools
as the hub's user while the extension is disabled, then enable it.

## Upgrade

```sh
personad extension upgrade personal-agent-club file:/path/to/personal-agent-club-<newer version>.tar.gz
```

The version must be higher than the installed one. The hub stops the old program and starts the new one, keeping the
settings and the data directory; when the new one does not come up within 30 seconds it goes back to the old one by
itself. An upgrade that asks for more (another host, another contribution place) shows what grew and asks again. No
hub restart.

A package with the **same version** as the installed one cannot be upgraded to. To run its programs anyway, remove
the installed one keeping its data, then install again; this drops the extension's settings (set `demo_echo` again
if you used it), not its data directory:

```sh
personad extension uninstall personal-agent-club --keep-data
personad extension install file:/path/to/personal-agent-club-<version>.tar.gz
```

## Roll back or remove

```sh
personad extension rollback personal-agent-club                 # back to the version before the last upgrade (one step)
personad extension disable personal-agent-club                  # stop it; settings and data stay
personad extension uninstall personal-agent-club --keep-data    # remove it but keep the data directory
```

`uninstall` without `--keep-data` deletes the data directory, key file included. To go back to a state you backed up,
uninstall with `--keep-data`, put the saved data directory back in place, and install the matching package again.
