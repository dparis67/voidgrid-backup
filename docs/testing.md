# What has been tested against real storage

The automated tests (`scripts/go.sh test ./...`) use local directories and an
in-process SFTP server. Separate **live suites**, each behind a build tag and run
by a script in `scripts/` with credentials from `.env`, use real storage. This
page says what each live suite covers, what no test has covered, and why, so you
know how far to trust each backend. The README's "What the live tests need" says
how to run them.

## SFTP: a Hetzner Storage Box

`scripts/sftp-live-test.sh` runs against a real Storage Box, in a throwaway
directory it deletes afterwards.

- **Covered:** creating a repository (trusting and pinning the host key), a
  backup, a full check with a test restore, a restore compared byte for byte,
  and **moving** a repository by renaming its directory: the old path is empty,
  the same repository opens at the new one, a move onto an existing directory is
  refused, the next backup is fully incremental, the agent's cache follows, and a
  restore still matches.
- **Not covered:** any other SFTP server (the Storage Box's behaviour on a
  directory rename is the one that was checked), password login, several agents
  using one Storage Box at once against its connection limit (the server's
  SFTP connection limit is tested with counters and a fake store, not against a
  real box), the parallel wipe against a real box, server-run maintenance over
  SFTP, and repositories large enough for listing or deleting blobs to take
  hours.

## S3: Backblaze B2 only

`scripts/s3-live-test.sh` runs two suites against a Backblaze B2 bucket through
its S3-compatible API, under a throwaway prefix it deletes afterwards:

- `TestLiveS3Suite` (the engine): create, reconnect to what is there, a wrong
  secret key being refused, incremental backups, listing and browsing, a full
  check with a test restore, a restore compared byte for byte, deleting a
  snapshot, measuring, maintenance, and a wipe.
- `TestLiveS3Server` (the server, with a real in-process agent): adding a
  repository through the agent, who can open it, a job backed up twice, measuring,
  maintenance, a credentials edit that is refused when the new secret does not
  work and leaves the saved settings alone, deleting snapshots through the agent
  and through the server, wiping through the agent, and creating a repository
  from the server, maintaining it and wiping it through the server.

These suites are **deliberately limited**, for these reasons:

- **A free account.** The runs use a few kilobytes of fake data and one
  repository each, and avoid repeated setup, because a free Backblaze account
  has a daily allowance and counts listing and download requests separately. The
  number of requests a run makes has not been measured, so check the provider's
  usage page after a run. If the allowance runs out, requests are refused and a
  run fails for a reason that has nothing to do with the code.
- **One provider.** Nothing has been run against Amazon S3, Wasabi, MinIO or any
  other S3-compatible service, so the S3 support is only known to work with
  Backblaze's S3 API.
- **One working key.** The success path of editing credentials is untested,
  because that needs a second valid key. Only the refusal of a bad secret is.
- **A move is not offered.** S3 has no rename, so a bucket, endpoint or prefix
  cannot be edited and no move test exists. See "Why an S3 repository cannot be
  moved" in [server.md](server.md#editing-a-repository).

Also **never tested on S3**: a stack or a VM backup and its restore, two agents
sharing one repository, restoring on a different agent, repositories with many
thousands of objects (listing, measuring and wiping grow with the object count,
and the 12 hour wipe limit has never been reached), and whether wiping or
deleting frees space on a bucket set to keep old versions.

## A directory on the agent host

Covered by the automated tests on local temporary directories only. A network
mount (NFS or SMB) has not been tried; testing or assigning a filesystem
repository warns when its directory is not on a network mount, and the server's
access to the same directory through a mapped path is only tested on one machine.
