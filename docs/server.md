# voidgrid-backup-server

The server is the single pane of glass: web UI, JSON API, scheduler and the
catalog of agents, repositories, jobs and run history. It never reads or
writes backup data itself: it tells agents what to do over mutual TLS, and
agents talk to the repository directly.

## Quick start

```sh
cp examples/server/.env.example examples/server/.env   # then edit it
cd examples/server
mkdir -p data && sudo chown 1000:1000 data
docker compose up -d
```

`VB_UID`/`VB_GID` default to `1000:1000`, the usual first non-root user on
a Linux host — if that's already you, `./data` often needs no `chown` at
all. Set them to your own user (`id -u`/`id -g`) instead if it isn't: the
image doesn't care which uid it runs as, only that it can write `./data`.

Open `http://<host>:<VB_PORT>/`. The first visit is always to `/setup` —
see [Authentication](#authentication) below before you get there, it needs
a token from the log. Then register an agent (see [agent.md](agent.md)) and
approve it on the Agents page, add a repository, and add jobs.

## Configuration

Every setting is a flag and an environment variable; a flag given on the
command line wins over the environment.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-listen` | `VB_LISTEN` | `:8080` | HTTP address for the UI, API and `/healthz` |
| `-agent-listen` | `VB_AGENT_LISTEN` | `:9442` | TLS address agents register on. In the example compose `VB_AGENT_PORT` sets this and the published port together. |
| `-data` | `VB_DATA` | `/data` | Directory for `catalog.db`, `pki/` and `session.key` |
| `-poll` | `VB_POLL` | `30s` | How often every agent is checked |
| `-oidc-issuer` | `VB_OIDC_ISSUER` | (empty) | OIDC provider issuer URL. Empty means sign-in is by recovery code only — see [Authentication](#authentication) |
| `-oidc-client-id` | `VB_OIDC_CLIENT_ID` | | OIDC client ID |
| `-oidc-client-secret` | `VB_OIDC_CLIENT_SECRET` | | OIDC client secret |
| `-oidc-redirect-url` | `VB_OIDC_REDIRECT_URL` | | Callback URL exactly as registered with the provider, e.g. `https://backup.example.com/auth/callback` |
| `-oidc-allowed-emails` | `VB_OIDC_ALLOWED_EMAILS` | (empty) | Comma-separated allowlist. Empty allows any identity the provider authenticates |
| | `TZ` | `UTC` | Time zone for schedules and displayed times |
| `-cache-dir` | `VB_CACHE_DIR` | | Base directory for Kopia's local cache of the repositories the server maintains itself, one subdirectory per repository. Empty keeps it under `-data`. |
| `-cache-metadata-mb` | `VB_CACHE_METADATA_MB` | `256` | Size limit in MiB, per repository, of the metadata cache |
| `-cache-content-mb` | `VB_CACHE_CONTENT_MB` | `64` | Size limit in MiB, per repository, of the file-data cache. `0` turns the whole cache off. |

Job schedules are evaluated in the server's `TZ`, and so is every time the
interface shows (browsers' own zones are not used, so a cron expression and the
times around it always agree). The header of every page names the zone, for
example `America/Chicago · CDT (UTC-05:00)`, and the schedule fields say which
zone they are in. The time zone database is built into the binary, so `TZ`
works in the minimal image. Set `TZ` to your own zone, as `UTC` is only the
default.

> **Upgrading from v0.9.0-beta.13 or earlier:** those builds read every cron
> expression in UTC, whatever `TZ` was. From v0.9.0-beta.14 they are read in
> `TZ`, as described above. A job set to `0 1 * * *` to run at 20:00 Central
> now runs at 01:00 Central. Change each job's schedule to its local hour
> (`0 20 * * *`) when you upgrade, and until you have, expect one run at the
> old expression's local hour: the next run is counted from the last one.
>
> From v0.9.0-beta.15 the Jobs page offers a one-time **conversion**: a preview
> lists every scheduled job with its schedule now and after converting (the
> same clock time as before, at today's UTC offset, moving the weekdays too
> when an hour crosses midnight). Untick a job you already set in local time,
> then convert. Schedules that are not a single daily time (`*/15`, several
> hours, ranges, a day of the month) are listed but must be edited by hand. The
> conversion can be done once, and it is recorded so it can't shift schedules
> twice; there is also a button to hide the offer without converting. A server
> whose zone is UTC is never offered it.

## The data directory

| Path | What it is | If you lose it |
|---|---|---|
| `catalog.db` | SQLite: agents, repositories (including their passwords), jobs, runs | You have to add repositories and jobs again. The backups themselves are safe in the repositories, if you still have their passwords. |
| `pki/ca.crt`, `pki/ca.key` | The CA every agent trusts | Every agent has to register again |
| `pki/registry.crt`, `pki/registry.key` | Certificate of the agent registration listener; the registration token pins it | Recreated, which changes the token: update `VB_TOKEN` on agents that are not approved yet |
| `pki/server.crt`, `pki/server.key` | The server's client certificate for talking to agents | Recreated from the CA |

`catalog.db` holds repository passwords in plain text, so the directory is
created `0700` and the database `0600`. Back this directory up somewhere that
does not depend on the server.

## Health check

`GET /healthz` returns `200 ok` when the server is up and its catalog answers
a query, `503` otherwise. The image has no curl, so the compose health check
runs the binary's own probe:

```sh
voidgrid-backup-server healthcheck   # exit 0 healthy, 1 unhealthy
```

It reads the same `VB_LISTEN` and probes `/healthz` on loopback.

## Network and security

- Agents register with the server on its own TLS listener (`VB_AGENT_LISTEN`,
  default `:9442`), separate from the web UI port, so the UI can stay behind a
  reverse proxy. Its certificate is self-signed; the registration token pins it.
  A valid token only creates a pending entry that you approve or reject.
- After approval the server connects **to** the agent (default port 9443), so
  the server must be able to reach each agent's address. It accepts only the
  exact certificate issued at approval, so after an agent is replaced the old
  host's certificate stops working.
- Wrong-token attempts on the registration port are logged once per source
  address per minute, followed by one line with the count, so they can't
  flood the log.
- Agent traffic is mutual TLS with the server's own CA. An approved agent
  accepts only the server's client certificate.
- Cross-site form posts are refused (Go's `net/http.CrossOriginProtection`),
  so another site in your browser can't trigger actions here even while
  you're signed in.

## Authentication

**Signing in is always required — there is no "every route is open" mode.**
OIDC (Authelia, Authentik, Keycloak, Google, etc.) is one way to sign in,
configured through the setup wizard or the **Authentication** page — not
only `-oidc-issuer` and friends (see the table above), though those work
too, and always win over whatever's configured through the UI (see
[Configuring OIDC](#configuring-oidc)). Not configuring it doesn't disable
sign-in, it just means the only way is a recovery code (below). This
doesn't depend on or interact with any reverse proxy's own auth; it's the
server's own, so publishing it directly still requires signing in either
way.

When OIDC is configured:
- Anyone who completes the provider's login is let in, unless an allowed-
  identities list names specific ones — worth setting once more than one
  person could reach the provider.
- The sign-in chooser (`/auth/login`) offers **Sign in with SSO** alongside
  **Use a recovery code**, so a broken provider is never something you have
  to actually reach to see the alternative.

Without it, the chooser only offers the recovery code — still a real,
supported way to run this on a single-host homelab where standing up an
OIDC provider is more infrastructure than the backup tool itself warrants.

Either way:
- The session is a signed cookie, valid 30 days, backed by a key generated
  on first use and stored at `<data>/session.key`. Losing it (or restoring
  a backup from before it existed) just signs everyone out; it holds no
  backup data or secrets from your OIDC provider.
- `/healthz` stays open, for the container health check.
- The JSON API returns `401`, not a redirect, when not signed in.
- Signing out clears the local session only; it doesn't call back to an
  OIDC provider to end that session there too.

### Setup wizard and recovery codes

Every install needs at least one way to sign in that doesn't depend on
external infrastructure — otherwise a wrong OIDC client secret, an
unreachable issuer, or a `redirect_url` mismatch would lock out every route
with no recourse short of editing `.env` and restarting the container. So
the very first thing any install does, OIDC or not, is generate 10
one-time recovery codes, the same idea as a TOTP app's backup codes.

**The first time the server starts:**

1. It logs a one-time **setup token** (look for `setup_token` in the
   container's logs) and refuses to let *anything* else through — the UI,
   the API, the OIDC redirect if one is configured — until setup finishes.
   `/healthz` is the one exception, so the container's own health check
   doesn't fail during this window.
2. Visit `/setup`, enter that token, optionally fill in an OIDC provider
   right there (or leave it for later — see [Configuring
   OIDC](#configuring-oidc)), and submit. The server generates 10 recovery
   codes, shown **once** — record them somewhere that doesn't depend on
   this server (a password manager, or paper), the same as a repository
   password.
3. From then on, `/` redirects to the sign-in chooser (`/auth/login`).

**A recovery code**, entered at `/auth/recovery`, signs you in exactly like
a successful OIDC login would. If no OIDC provider is configured, this is
the *only* way to sign in — not just a fallback. Each code works once;
reusing one is rejected. Once signed in (by either path), visiting `/setup`
again lets you generate a fresh batch of 10, which immediately invalidates
every code in the old one, used or not — do this after using a few, or if
you suspect they've leaked.

`/setup` is unreachable to an anonymous visitor once codes already exist —
that's the "unavailable otherwise" half of it. Nobody who reaches the
server after you've completed it can mint themselves a fresh batch without
already being signed in.

### Configuring OIDC

Set it up during the wizard, or later from the **Authentication** page
(signed in, since it's a normal settings page at that point) — same
fields either way: issuer URL, client ID, client secret, redirect URL
(exactly as registered with the provider), and an optional comma-separated
allowed-identities list. Turning OIDC on, off, or pointing it at a
different provider **takes effect immediately, no restart** — the change
is validated (the server actually performs OIDC discovery against the
issuer you gave it) before it's saved, so a typo is caught right there on
the form instead of surfacing later as a failed restart.

`-oidc-issuer` and friends always win when given, exactly as before: a
server started with those flags/env vars shows its OIDC settings as
"set by environment variables, not editable here" on both pages, and
managing OIDC through the UI only takes effect once they're removed.

If OIDC was working when last configured but its issuer is unreachable at
a later **startup** (network down, the provider moved), the server logs
the failure and falls back to recovery-code-only sign-in rather than
refusing to start — a process that won't boot at all is a worse lockout
than OIDC being temporarily unavailable.

## Using the UI

### Agents

Agents register themselves (see [agent.md](agent.md#registration)) and wait
under **Waiting for approval**. Each entry shows the agent's key fingerprint;
approve only if it matches the one in that agent's log. Approve one with a name and the address the
server can reach it on (`host:port`, pre-filled from where it connected and
the port it reported); the address is editable. Choosing an existing agent
under **Replaces** makes the new registration take over that agent's ID, jobs
and snapshots, which is how a wiped or redeployed host comes back. Reject
refuses an agent; Forget removes a rejected entry so it can register again.
Agents can be renamed at any time: the name is only a label.

The **Registration token** is shown on the same page. It is one reusable
secret for all new agents, rotatable with a button; rotating makes the old
token stop working and drops agents that are still waiting, but agents that
are already approved are unaffected. `voidgrid-backup-server token` prints it
on the command line, for deploy scripts.

The table shows reachability, version and last contact, plus any warnings the
agent reports about itself (for example running without the permissions it
needs).

Each agent has two discovery pages:

- **stacks**: the Docker compose projects on that host, for stack jobs.
- **VMs**: the libvirt domains on that host, for VM jobs.

### Repositories

A repository is where snapshots are stored. The chosen agent creates it (or
connects to one already at that location) and becomes its maintenance owner:
the agent that runs Kopia's compaction and garbage collection.

| Kind | Fields | Notes |
|---|---|---|
| SFTP | host, port, user, path, key file or password, known_hosts | Hetzner Storage Box: port `23`, user `uXXXXXX`, a path relative to its home such as `backups/homelab`. The key file is a path **inside the agent** (e.g. `/keys/sftp_key`). Leave known_hosts empty to trust the key seen on first connect; it is pinned from then on. |
| S3 | endpoint, bucket, prefix, region, access key, secret | "Plain HTTP" only for a local test server. |
| Filesystem | directory on the agent host | A local disk or a mount on that agent. |

- **Password**: leave it blank to have one generated. It is shown **once**.
  It is the only key to the data: record it somewhere that doesn't depend on
  your hosts (password manager, paper) — **and record the connection
  details you enter here too** (host/bucket, path, user, key or
  credentials). The password alone isn't enough to recover with the server
  gone; see [recovery.md](recovery.md) for what `voidgrid-backup-recover`
  needs and why.
- **Reed-Solomon error correction** (bitrot protection) costs about 2% space
  and can only be chosen when the repository is created.

#### Using a repository from another agent

Every request the server sends to an agent carries the repository's
connection details and its password, so an agent that has never seen the
repository can still open it. The password is stored once, on the server;
you don't copy it to agents. What an agent needs on its own depends on the
kind:

| Kind | Needed on the other agent |
|---|---|
| S3 | Nothing. The access key and secret are part of the repository. |
| SFTP with a password | Nothing. |
| SFTP with a key file | The key file at the **same path** as in the repository (e.g. `/keys/sftp_key`), and its public key authorized on the SFTP server. See [SFTP key naming](agent.md#sftp-key-naming). |
| Filesystem | The same directory reachable from that agent (a shared mount). |

This is what makes a restore on a different agent work (below). To back up
from several agents **into** one repository, see
[Sharing a repository between agents](#sharing-a-repository-between-agents).

#### Sharing a repository between agents

Kopia is built for several clients writing to one repository, and the identity
of each snapshot stays per agent (`voidgrid-backup@<agent ID>`), so agents'
histories and retention never touch each other.

- **Assign** an agent on the Repositories page (**Assign an agent...** on the
  row). The agent first opens the repository, never creating anything, and is
  assigned only if that works; the same form re-tests an assigned agent. A job
  can only be created for an agent assigned to its repository. Existing
  installs were migrated so every agent that already had a job is assigned.
- **Default repository.** Each agent has one default, which the add-job forms
  preselect (agent → stacks or VMs, and Jobs → path job, which follows the
  agent you pick). An agent's first repository becomes its default; change it
  with **Make default** beside the agent. When an agent can use several
  repositories and none is its default, the form asks you to choose.
- **Maintenance when adding a job.** The add-job forms have "Maintenance of this
  repository": leave it, or make the server or the job's agent the maintainer
  (the same change as **Maintainer...** on the Repositories page). It is set
  per repository, so it applies to every job on it, and it is refused while a
  job on the repository is running; the job is created either way and the
  notice says what happened.
- **Keys.** An SFTP repository stores a default key file path; give an agent
  its own with **Key file on that agent**, and authorize each agent's public
  key for the same SFTP user (one `authorized_keys` line per agent, see
  [SFTP key naming](agent.md#sftp-key-naming)). The key is tested when you
  assign. Agents only ever receive their own path.
- **Unassign** is refused while the agent has jobs on the repository.
- **Maintenance**: let the server maintain a repository that several agents
  share ([above](#letting-the-server-maintain-a-repository)); an agent owner
  that is replaced would otherwise stall it. The Repositories page says so for
  a shared repository that an agent still maintains.
- **Default location.** A new repository's SFTP path and S3 prefix start as
  `<agent-id>-backup`, so unrelated data never shares a directory. It is a
  default, not enforced.
- A Storage Box limits simultaneous connections (about 10). Jobs on several
  agents share them, so stagger their schedules (the Jobs page warns about
  overlaps).
- Several agents writing to one repository has only been tested between
  in-process agents on a local filesystem repository, not against a real
  Storage Box or S3.

#### Maintenance

Maintenance (compaction and garbage collection) is run by the repository's
maintenance owner, and only by it. It runs at the end of each backup when
Kopia's own schedule says it is due, and in addition the server runs a **full**
cycle every 24 hours (plus a random 0-1 hour, so repositories don't all start
together; the first runs after a server start are spread over 2 hours) for each
repository that has at least one job. A repository with a running job waits.
A repository without jobs is never scheduled. The **Run now** button on the
Repositories page runs a full cycle on demand and works for every repository.
On a job's page, tick **then run maintenance** next to **Back up now** to back
the job up and then run a full cycle on its repository, due or not; it is
skipped if the backup fails. Every cycle is recorded with how long it took and
how long it waited for its agent: the Repositories page lists the last 10, and
the Schedule page draws the next one as a striped bar on the agent's lane. A
cycle holds its agent like a backup, so jobs on that agent queue behind it.

The server asks an agent with a job on the repository; if that agent is not the
owner it names the owner (the owner's host name is its agent ID) and the server
asks that agent. If the owner is no longer a registered agent, for example a
host that registered again without "replaces", the row shows that and no agent
can run maintenance.

Running maintenance does **not** free space at once. Kopia keeps deleted data
for at least 24 hours and needs two garbage-collection cycles, so space from
deleted snapshots comes back after about two days of daily runs.

#### Letting the server maintain a repository

By default the agent that owns a repository runs its maintenance. Because the
owner is an agent, replacing or re-registering that host leaves the repository
without a maintainer. A repository can instead be maintained by the server,
whose identity (`voidgrid-backup@voidgrid-server`) does not change:

- Repositories page, **Maintainer...** on the row: pick "server", type the
  repository name, press Change. The server first opens the repository itself
  (it never creates anything), then makes itself the Kopia maintenance owner.
  Nothing else changes; backups still run on the agents.
- It is refused while a job on the repository, a maintenance run or a wipe is
  running. Kopia has no repository-wide maintenance lock, so the owner is only
  changed while nothing is running.
- The server needs to reach the storage itself. S3 and SFTP with a password
  work as they are. SFTP with a key file needs that key at the same path inside
  the **server** container; the test in the previous step tells you whether
  it works.
- A **filesystem** repository is a directory on an agent host. Give its path
  as the server sees it (**Path on the server**, optional) if the same
  directory is mounted in the server container; leave it blank and the
  repository stays maintained by its agent. Use the same share, mounted the
  same way, on both: NFS and SMB caching can show one side stale data for a
  moment. The agents never receive this path. Before switching, an agent that
  uses the repository opens it at its own path and must report the same
  repository ID as the server read through the mapped path; if the IDs differ,
  or no agent can answer (offline, or older than the server), nothing changes.
- The server's clock must be right: Kopia refuses to run maintenance when the
  server's time and the repository's differ too much.
- Choosing "an agent" has that agent open the repository and make itself the
  owner. The server does not need to reach the repository for this, so it works
  even when the server has lost access. An agent older than the server answers
  that it needs updating.

#### Removing backups

- **Delete a snapshot**: on the job page, type its short ID in the box on its
  row and press Delete. Only snapshots of that job's agent can be deleted. The
  data is released later by maintenance (see above).
- **Remove a repository**: Repositories page, Remove... section. Takes the
  repository off the server and leaves its data in storage, so it can be added
  again with its password. Refused while any job uses it.
- **Wipe a repository**: same section. Deletes every blob of the repository from
  storage through the agent you pick, then removes it from the server. Needs no
  repository password, cannot be undone, and frees the space at once. It refuses
  when no repository is found at the location, and while any job uses it or the
  repository is busy. Empty directories remain on SFTP and filesystem storage.
  If it fails, the error stays on the row and it can be run again.

All of these ask you to type the name (or snapshot ID) and are logged at
`WARN` on the server and the agent.

### Jobs

There are three kinds of job, all with a cron schedule (standard 5 fields,
blank for manual only) and retention counts (keep latest/hourly/daily/
weekly/monthly/annual):

- **Path job** (Jobs page): absolute directories on one agent, plus
  gitignore-style excludes. `/`, `/proc`, `/sys`, `/dev` and `/run` are refused.
- **Stack job** (agent → stacks): one compose project. See
  [agent.md](agent.md#compose-stacks).
- **VM job** (agent → VMs): one libvirt domain. See [agent.md](agent.md#virtual-machines).

A job page has "Back up now", "Verify repository", the schedule switch, run
history and the snapshot list. Deleting a job removes it and its run history;
its snapshots stay in the repository unless you tick **Also delete its
snapshots**, which forgets every snapshot in the job's list first (and keeps the
job if that fails). That cannot be undone, and the space is released by the next
full maintenance cycle.

**Verify repository** runs a check on demand: it confirms every snapshot's
metadata and directory listings, that every content blob still exists, spot
checks (~10%) of file contents by downloading, decrypting and hashing them,
and test restores the newest snapshot of the job's smallest source (up to
500 MiB) to a scratch directory to confirm the restored file count and size
match what was recorded. With Reed-Solomon error correction on, bitrot found
along the way is repaired automatically. It shares the job's run lock, so it
can't overlap a backup or restore of the same job, and shows up in run
history as kind `check`.

### Runs

| Status | Meaning |
|---|---|
| success | Everything was backed up / restored |
| partial | It finished, but something needs attention: unreadable files (named), a failed database dump, a crash-consistent VM copy, ownership that couldn't be restored, or **ACTION NEEDED** for a VM disk left on an overlay |
| failed | Nothing usable was produced |
| running | In progress. Runs still "running" when the server restarts are marked failed on start. |

### Browsing and restoring

Click a snapshot to browse it. From any directory you can restore that
directory (or the whole snapshot) as files to a directory on the agent.
Without "overwrite" the target must be empty or not exist yet, so a restore
never half-completes over existing data.

Stack and VM snapshots also offer a whole-stack / whole-VM restore and, for
stacks, "import dump". In-place restores ask you to type the stack or VM name.

#### Restoring on a different agent

With more than one agent enrolled, the snapshot page has a **Browse and
restore on** picker. It defaults to the job's own agent. Pick another one to
browse the snapshot and restore it there, for example when the job's host is
gone or you want the data on a new machine. The snapshot list comes from the
server, so the page opens even when the job's agent is down.

- A restore on another agent always goes into a **target directory** (empty or
  not yet existing, as usual). "Original locations" and in-place restores of
  stacks and VMs run only on the job's own agent.
- Stack and VM restores into a directory write the snapshot's files there
  (`stack/`, `mounts/`, `dumps/`, or the disks and `domain.xml`) and touch no
  containers or VMs, so that agent needs neither Docker nor libvirt. Putting
  the stack or VM back into service afterwards is manual.
- The other agent must be able to open the repository (see
  [above](#using-a-repository-from-another-agent)) and have a writable
  directory for the target ([Paths and read-only mounts](agent.md#paths-and-read-only-mounts)).
- The run is recorded under the original job, and its summary names the agent
  it ran on.

#### Restoring on the server

When the job's agent is broken or gone and no other agent suits, the same
picker offers **The server**. The server then reads the repository itself and
writes the restored files into a directory **inside its own container**, so
mount a volume at the path you restore to, or the files vanish with the
container. It needs no agent, Docker or libvirt.

- It is offered only when the server can open the repository: S3, SFTP with
  the key mounted in the server container (see
  [Letting the server maintain a repository](#letting-the-server-maintain-a-repository)),
  or a filesystem repository with a **Path on the server**. Otherwise the
  option is greyed out and its tooltip says why.
- Like any other restore on a different host it always goes into a target
  directory; in-place restores stay on the job's own agent.
- Files are owned by the user the server runs as, and ownership and special
  permissions that need root are not restored unless it runs as root.
- To restore only part of a stack or VM snapshot (say one data folder), browse
  into it and use **Restore as files** on that folder; the "Restore stack"
  form restores the whole snapshot.
- If the server itself is gone too, use
  [voidgrid-backup-recover](recovery.md).

### Schedule

The **Schedule** page draws a window of the day as a timeline, one lane per
agent (or per repository with the link above it). Each scheduled job is a bar
from its start time for as long as its backups take; click a bar to open the
job. The line marks now, and bars that have already started are dimmed.

- **Zoom** the window to 5 or 15 minutes, or 1, 3, 6, 12 or 24 hours, with the
  buttons above the timeline (24 hours is the default). The 5 and 15 minute
  views are for jobs that last seconds. The window starts at the beginning of
  the current hour, or of the current 5 or 15 minutes in those two views.
  **Earlier** and **later** move it by its own length, **now** goes back to the
  current hour, and the **Start at** field sets an exact time. The zoom and
  start are in the page's address (`span` is the length in minutes), so a link
  or a refresh keeps the view. A bar is never drawn narrower than a few pixels,
  and a name that does not fit in its bar is written beside it when there is
  room (otherwise it is in the hover text). A
  run that began before the window and is still going at its left edge is
  drawn from the edge.
- A job's length is the median of its last 20 finished backups (the shorter
  of the middle two for an even count), not counting the time a run waited for
  its agent (runs from before that was recorded still include it). The first
  backup uploads everything and is far longer than the rest, so a median lets
  it stop mattering after a second or third run; hover a bar for the longest
  run. A job with one finished backup uses it. A dashed bar has no finished
  backup yet and assumes 15 minutes. Maintenance bars use the median of the
  last 10 cycles the same way.
- Jobs on one agent run one after another, so overlapping bars in an agent
  lane wait for each other. Jobs on one repository share its connections and
  bandwidth. Each job has its own row in its lane (jobs in name order, then
  maintenance), with every run of it on that row, so a job keeps its row when
  you drag it. Bars that overlap in time are drawn in the warning colour.
- **Drag a bar sideways** to change when its job starts, in 5 minute steps (1
  minute in views of 3 hours or less).
  The bar's job gets a new minute and hour in its cron schedule, and it
  applies from the next scheduled start. This works for a schedule with one
  start time a day (`0 20 * * *`, or limited to weekdays such as
  `0 20 * * 1,3`); dragging past midnight moves the weekdays with it. Other
  schedules (`*/15`, several hours, ranges, a day of the month, `@daily`) are
  not draggable and are edited on the Jobs page. A drag made from a page that
  is out of date is refused, so it can't overwrite a newer edit.
- Manual and switched-off jobs are listed under the timeline.

### Notifications

One server-wide setting, not per-job: a **scheduled** job that finishes
`partial` or `failed` is sent to whichever of these are enabled. A manual
run never notifies — you're already watching it in the UI — and neither
does a success.

| Channel | What it needs |
|---|---|
| Discord | A webhook URL from a channel's Integrations settings. Posts directly, no third party involved. |
| [Notifarr](https://notifiarr.com/) | An API key and the numeric Discord channel ID to deliver to, from Notifarr's Passthrough integration. Goes through Notifarr rather than posting to Discord directly, so it shows up alongside whatever else you already route through it. |
| Email | An SMTP server: host, port, optional username/password, a from address and at least one recipient. Port 465 is implicit TLS; anything else tries STARTTLS if the server offers it. |

All three can be on at once. A secret field (webhook URL, API key, SMTP
password) left blank when saving keeps whatever was already stored for that
channel — the way to turn a channel off is to uncheck "enabled," not to
blank its secret. "Send test notification" fires one test message to every
currently-enabled channel and reports each result, without needing a real
failure to test with.

## JSON API

Read-only, and behind the same login as the UI when one is configured (see
[Authentication](#authentication)) — a request with no valid session gets
`401`, not a redirect:

| Endpoint | Returns |
|---|---|
| `GET /api/agents` | Agents with status and warnings |
| `GET /api/repositories` | Repositories, secrets redacted |
| `GET /api/jobs` | Jobs and their configuration |
| `GET /api/jobs/{id}/runs` | The last 100 runs of a job |

## Upgrading

Pull the new image and recreate the container. The catalog schema migrates
itself on start. Upgrade agents too: an agent older than the server may not
know newer RPCs, which shows up as an error on the run.

## Troubleshooting

| Symptom | Look at |
|---|---|
| Agent "unreachable" | The error under it on the Agents page; can the server reach `host:port`? Was the agent's data directory lost (then register it again, approving it as replacing this one)? |
| An agent never appears under "Waiting for approval" | Its `VB_SERVER` reaches the registration port, and its `VB_TOKEN` is the current one (rotating changes it) |
| Schedules run at the wrong hour | `TZ` in the server's `.env` |
| A run is "partial" | The run's detail lines say exactly what and where |
