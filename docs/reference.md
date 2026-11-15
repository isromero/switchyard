# Reference

## How a pick works

1. **Self-hosted runners.** A self-hosted provider is picked if a runner with
   its label is online and idle, and there are more idle runners with that
   label than jobs already queued for it. The runner running the `pick` job
   itself counts as idle. Runs started by a pull request from a fork never
   get a self-hosted runner, since that would run outside code on your
   machine.
2. **Free minutes.** A hosted provider is picked while its free minutes this
   month, minus a safety margin, are not used up. A provider with
   `ignore_public_repos` (GitHub-hosted) is always free in a public
   repository. A hosted provider that looks stuck is skipped (see below).
3. **Paid.** Providers with `allow_paid: true` are next, up to `cap_minutes`
   and/or `cap_usd`. `cap_minutes` also limits the free pass. They are tried in
   `paid_order`, or in list order if that is not set.
4. **Fallback.** On any error (API down, bad token, broken config) the
   result is `fallback`; the step still succeeds and logs a warning. When
   every provider is known to be used up, `on_exhausted` decides:
   - `fallback` (default): use the fallback anyway. With `ubuntu-latest` that
     run is billed by GitHub, up to the budget set in your GitHub billing
     settings. The `paid` output is `true` and the log says so.
   - `fail`: the pick step fails and the workflow stops, unless the fallback
     is known to be free (a public repository, or a self-hosted label). The
     extra per-size outputs (`runner_medium`, ...) are left empty for sizes
     that are used up, so jobs using them fail instead of being billed.

### When a provider never starts the job

`runs-on` only names a label; switchyard can't see whether the provider will
actually start a machine for it. If the provider's app is not set up for the
account, its credit ran out or it is down, the job just waits in the queue.
GitHub cancels it after 24 hours, and `timeout-minutes` doesn't count queue
time.

switchyard can't rescue that job, but it notices it in the usage scan. A
hosted provider that left a job waiting at least `stuck_after` (default
`10m`) without a runner, and has run nothing since, is skipped. It is tried
again once a job runs on it, or an hour after the waiting job ended (cancel
it to get there). `switchyard usage` shows such a provider as `stuck`. Set
`stuck_after: 0` to turn this off. Self-hosted runners are not affected: they
are only picked when one is idle.

Each step only does the API work it needs. For example, if a self-hosted
runner is idle, usage is not looked at.

## Running the pick job on your own machine

The pick job itself can run on a self-hosted runner, so it uses no hosted
minutes:

```yaml
jobs:
  pick:
    # Give a shared label (here "switchyard") to every machine that can run
    # the pick job, so it isn't stuck waiting when one machine is down.
    runs-on: [self-hosted, switchyard]
    timeout-minutes: 2
    outputs:
      runner: ${{ steps.pick.outputs.runner }}
      big: ${{ steps.pick.outputs.runner_medium }}
    steps:
      - id: pick
        uses: isromero/switchyard@v1
        with:
          token: ${{ secrets.SWITCHYARD_TOKEN }}
          runners-token: ${{ secrets.SWITCHYARD_RUNNERS_TOKEN }}  # only for self-hosted
          arch: x64          # this project needs x86; drop it to allow arm64 too

  test:
    needs: pick
    runs-on: ${{ needs.pick.outputs.runner }}
    steps:
      - uses: actions/checkout@v4
      - run: make test

  build:
    needs: pick
    runs-on: ${{ needs.pick.outputs.big }}
    steps:
      - uses: actions/checkout@v4
      - run: make build
```

## One runner per job

By default every job that reads `runner` gets the same label. A self-hosted
runner runs one job at a time, so if the pick lands on it, the run's jobs
wait for it in turn. To spread them, list the jobs in the `jobs` input. Each
one gets its own label in the `runners` output (a JSON object). Idle
self-hosted runners go to the first jobs listed, one job each, and the
others go to the next provider. Hosted providers run jobs side by side, so
any number of jobs can share one.

`name*n` makes `name-1` ... `name-n`, for a matrix. `name:size` sets the
size for that job.

```yaml
jobs:
  pick:
    runs-on: ubuntu-latest
    outputs:
      runners: ${{ steps.pick.outputs.runners }}
    steps:
      - id: pick
        uses: isromero/switchyard@v1
        with:
          token: ${{ secrets.SWITCHYARD_TOKEN }}
          jobs: check, test:medium*5

  check:
    needs: pick
    runs-on: ${{ fromJSON(needs.pick.outputs.runners).check }}
    steps: [...]

  test:
    needs: pick
    strategy:
      matrix:
        shard: [1, 2, 3, 4, 5]
    runs-on: ${{ fromJSON(needs.pick.outputs.runners)[format('test-{0}', matrix.shard)] }}
    steps: [...]
```

With one idle self-hosted runner, `check` gets it and the five shards go to
the first hosted provider with free minutes. The labels are decided when the
pick runs, not when each job starts, and minutes are not split between
providers: all the hosted jobs go to the same one.

## Action inputs

| Input | Default | |
|---|---|---|
| `token` | (required) | Token for counting minutes; see below. |
| `runners-token` | `token` | Token used only to list self-hosted runners (needs admin read). |
| `download-token` | `token` | Token used to download the release binary. |
| `config` | see below | Path to a config file. |
| `size` | `small` | `small`, `medium` or `large`. |
| `arch` | `any` | `x64`, `arm64` or `any`. |
| `fallback` | config's `fallback` | Label used when nothing fits or anything fails. |
| `jobs` | | Jobs that each get their own runner, e.g. `check, test:medium*5`. See [One runner per job](#one-runner-per-job). |
| `disable` | | Comma-separated provider names to skip this time, e.g. `jog`. |
| `version` | from the ref | `v1.2.3`, `v1` (newest 1.x.y) or `latest`. A commit SHA ref uses the `vX.Y.Z` release tagged at that commit; if there is none, the pick falls back. |
| `actions-cache` | `false` | Keep the usage cache in actions/cache (see [Cache](#cache)). Ignored in public repositories. |

The config is looked up in this order:

1. The `config` input.
2. `.github/switchyard.yml` in the workspace. This only exists if the pick job
   checks out the repository.
3. The `switchyard.yml` at the root of the switchyard repository the action
   comes from. In your own fork you can keep one shared config there, and
   repositories don't need their own. The config is read at the ref you use: `@v1` reads it as of
   the latest v1 release, `@main` reads it from main.

## Action outputs

- `runner`: the label for the requested size.
- `runner_small`, `runner_medium`, `runner_large`: labels for each size, so one
  pick job can serve jobs of different sizes.
- `provider`: the name of the provider picked. Empty when the fallback was used.
- `paid`: `true` once the provider's free minutes are used up, so the run is
  billed. Also for a fallback in that state. Inside the safety margin it is
  still `false`.
- `fallback`: `true` if the fallback was used.
- `runners`: a JSON object with a label for each job in the `jobs` input.
  A job's label is empty when it has no runner (`on_exhausted: fail`, or a
  pull request from a fork with a self-hosted fallback), so that job fails.

## Things to know

- Pull requests from forks never run on self-hosted runners. If the
  fallback is a self-hosted label, such a run fails instead. This relies on
  switchyard running: if the binary can't be downloaded or the config can't
  be read, the action's `fallback` input is used as is, so in a public
  repository don't set that input to a self-hosted label.
- switchyard can't reserve a self-hosted runner. It counts jobs already
  queued for the label, so a runner that another run is about to use is not
  offered again. Two picks that start within the same few seconds can still
  both choose the same runner; the second run then waits for it. For
  org-level runners, queues are checked in the calling repository and in up
  to 50 org repositories with a push or a job in the last day or two.
- Org-level runners are only considered if their runner group allows the
  calling repository.
- If no runner with the pick job's own label is online, the pick job waits in
  the queue like any other job. switchyard can't help with that. Give the
  label to more than one machine, or run the pick job on `ubuntu-latest`,
  which costs about one billed minute per run.
- With `arch: any`, the first idle or free runner wins whatever its
  architecture. If your self-hosted runners are arm64 and a job only works on
  x86, pass `arch: x64`.

## Token and permissions

Tokens are read from the action inputs, or from `SWITCHYARD_TOKEN` (then
`GH_TOKEN`, then `GITHUB_TOKEN`) and `SWITCHYARD_RUNNERS_TOKEN` when you run
the CLI. They are never printed. `switchyard check` lists the permissions your
config needs.

The workflow's built-in `GITHUB_TOKEN` is not enough. It only sees the current
repository and can't list self-hosted runners. Create fine-grained personal
access tokens and save them as secrets in each repository that uses
switchyard. Personal accounts have no shared secrets; organizations do.

Use two tokens, so the one with admin rights does as little as possible:

**`token` (`SWITCHYARD_TOKEN`)**, read-only. Set the resource owner to the
account whose repositories should be counted. Give it access to all
repositories, or at least every repository whose minutes should be counted,
plus the switchyard repository if it is private.

- Repository **Actions: read**: list workflow runs and jobs, to count minutes
  and to see queued jobs.
- Repository **Metadata: read**: added automatically.
- Repository **Contents: read**: download the release binary. Only needed if
  the switchyard repository is private.
- Account **Plan: read** (user) or organization **Administration: read**: only
  for `billing_api`, to read GitHub's billing report. Without it switchyard
  logs a warning and uses its own estimate.

**`runners-token` (`SWITCHYARD_RUNNERS_TOKEN`)**, only if you use self-hosted
providers. It is used for one call: listing self-hosted runners, which GitHub
only allows to admins.

- Repository **Administration: read**, limited to the repositories that run
  switchyard.
- Organization **Self-hosted runners: read**, for `self_hosted.orgs`.

If you leave `runners-token` out, `token` is used for both, and then needs
both sets of permissions.

**Classic tokens** work too but can't be narrowed: `repo` (plus `user` for
`billing_api`, and `admin:org` for org-level runners or org billing).

If you host switchyard in a private repository, other repositories can only
use it as an action after you allow it. In the switchyard repository, go to
Settings → Actions → General → Access and choose to make it accessible from
repositories owned by the same user or organization.

## Configuration

See [`switchyard.yml`](../switchyard.yml) for a full, commented example. Check a
config with `switchyard check --config path/to/file.yml`.

```yaml
fallback: ubuntu-latest   # used when nothing fits or on any error
on_exhausted: fallback    # or "fail": stop the workflow instead of a billed fallback
stuck_after: 10m          # skip a hosted provider that left a job waiting this long (0 = off)
margin_minutes: 60        # stop using free minutes this early (weighted minutes)
paid_order: [depot, github]   # optional; priority for the paid pass

usage:
  accounts: [your-user]   # users/orgs whose repos are scanned (default: repo owner)
  repos: []               # or an explicit list of owner/name; replaces accounts
  cache_file: ""          # default ~/.cache/switchyard/usage-<hash>.json
  cache_ttl: 10m          # how old cached usage may be before a refresh
  timeout: 20s            # time limit for a refresh during pick

self_hosted:
  orgs: []                # also check these orgs' runners

providers:                # order = priority
  - name: jog
    kind: hosted          # hosted (default) or self-hosted
    enabled: true         # default true
    free_minutes: 5000    # per calendar month, weighted
    allow_paid: false     # keep using it after the free minutes?
    cap_minutes: 0        # hard monthly limit on weighted minutes (0 = none)
    cap_usd: 0            # hard monthly limit on estimated spend (0 = none)
    price_per_minute: 0.0012   # USD for a multiplier-1 minute
    margin_minutes: 60    # overrides the global margin
    ignore_public_repos: false # true for GitHub-hosted (free for public repos)
    billing_api: false    # GitHub-hosted only: count from GitHub's billing report
    runners:
      - label: jog        # the runs-on label
        size: small       # small (default), medium, large
        arch: x64         # x64 (default) or arm64
        multiplier: 1     # how much one minute counts (default 1)
      - { label: jog-4vcpu, size: medium, multiplier: 2 }
    count:                # extra labels to count (not to route to), glob patterns
      - { pattern: "jog-16vcpu", multiplier: 8 }
```

**Weighted minutes.** A minute on a runner counts as `multiplier` minutes. Use
1 for the provider's 2 vCPU Linux x64 runner, so a 4 vCPU runner is 2.
`free_minutes`, `cap_minutes`, `margin_minutes` and `price_per_minute` are all
in these units. Estimated cost is `(weighted − free_minutes) × price_per_minute`.

**Labels with the run id.** RunsOn labels must contain the run id. Write
`{{run_id}}` in the label and switchyard fills in `$GITHUB_RUN_ID`. When
counting, it matches any run id.

**Self-hosted providers** only use `runners[].label` (plus `size` and `arch`).
Their minutes are shown in `usage` but never limit anything.

**Disabling a provider for a while.** For example, Jog's free minutes only apply
while you have prepaid credit, and switchyard can't see your Jog balance. Set
`enabled: false`, or pass `disable: jog` to the action, or set
`SWITCHYARD_DISABLE=jog`.

## Providers

Labels for these providers are in [`switchyard.yml`](../switchyard.yml).
Free allowances and prices change, so check each provider's page and set
`free_minutes` and `price_per_minute` to what applies to your account.

| Provider | Notes |
|---|---|
| Self-hosted | Any label. Picked only when a runner with it is online and idle. |
| [Jog](https://usejog.com/docs/runners) | Free minutes only while the account has prepaid credit. |
| [Blacksmith](https://docs.blacksmith.sh/blacksmith-runners/overview) | GitHub organizations only. |
| [Ubicloud](https://www.ubicloud.com/docs/github-actions-integration/runner-types) | Needs a credit card on file. |
| [Depot](https://depot.dev/docs/github-actions/runner-types) | |
| [Namespace](https://namespace.so/docs/reference/github-actions/runner-configuration) | A label without a shape means 4 vCPU. |
| [WarpBuild](https://www.warpbuild.com/docs/ci/cloud-runners) | |
| [RunsOn](https://runs-on.com/runners/linux/) | Runs in your AWS account. |
| [GitHub-hosted](https://docs.github.com/en/billing/reference/actions-runner-pricing) | Free minutes depend on your plan. Public repos are free. |

### Adding a provider

1. Look up its `runs-on` labels, its free allowance and its price for the
   smallest Linux runner.
2. Add an entry under `providers` at the position that matches its priority.
   List one `runners` item for each size/arch you want to route to, with a
   multiplier relative to the 2 vCPU x64 runner.
3. If you have used other labels from that provider this month, add `count`
   patterns so those minutes are counted too.
4. Run `switchyard check`, then `switchyard usage`, and see that its jobs
   show up on its row and not under `(other)`.

No code change is needed. A job belongs to the first provider, in config order,
with a matching label, so labels must not overlap between providers. `check`
rejects exact duplicates.

## Counting minutes

switchyard lists every repository of the configured accounts. For each one it
lists the workflow runs created this month and in the 31 days before (runs
can be re-run for 30 days), then the jobs of every run, including re-run
attempts. Each job counts from `started_at` to `completed_at`, rounded up to
a whole minute as GitHub bills. Running jobs count up to now. Jobs that never
got a runner (still queued, cancelled while waiting, or not started because of
billing) don't count. A job belongs to the month it started in, using the UTC
calendar month. Provider billing cycles may differ.

This is an estimate. It is built from job durations and the multipliers in
your config, not from the providers' invoices, so it can drift: providers
round differently, some bill per second, and GitHub's own multipliers can
change. `margin_minutes` is there to absorb that. Two things help:

- For GitHub-hosted runners, `billing_api: true` counts from GitHub's billing
  usage report instead. Jobs from the last two hours are added from the
  estimate, in case the report doesn't have them yet. This matters after you
  move a repository to another account: its old runs stay in its history,
  but they were billed to the previous owner. `switchyard usage` shows both
  figures, so you can see how far apart they are.
- For other providers, compare `switchyard usage` with the provider's
  dashboard now and then, and adjust multipliers or the margin. switchyard
  can't see account state such as a prepaid balance running out (see
  "Disabling a provider" above).

### Cache

Results are kept in a small JSON file, by default
`~/.cache/switchyard/usage-<hash>.json` on the runner, one per set of scanned
accounts or repositories. The file holds raw job records,
so changing labels or multipliers takes effect without fetching again.
Completed runs are never fetched twice. Repositories with no runs this month
and no push since the last check are listed again at most once an hour. If a
refresh times out, everything fetched so far is kept, so the next pick
continues from there.

How long a pick takes depends on how many repositories and runs there are.
Rough numbers for about a hundred repositories and a few hundred runs a month:

- Cache younger than `cache_ttl`: under a second (only the runner check).
- Refresh: a few seconds.
- First scan of the month: tens of seconds. That can be longer than the 20 s
  `timeout`, so the first picks may fall back to `ubuntu-latest` while the
  cache fills in. To warm it, run `switchyard usage` once on the runner. It
  has a 5 minute timeout.

Usage that can't be known (no cache yet and the API failing) is treated as
unknown. Hosted providers are then skipped, never assumed to be free.

On ephemeral runners the local file is lost after each job. Set
`actions-cache: true` to keep it in actions/cache instead. Caches are per
repository, so each repository warms its own. This only works in private
repositories: the file lists private repositories and their usage, and pull
requests from forks can read a public repository's caches.

## CLI

```
switchyard pick  [--config f] [--size small|medium|large] [--arch x64|arm64|any]
                 [--fallback label] [--disable a,b] [--repo owner/name] [--refresh]
switchyard usage [--config f] [--refresh] [--timeout 5m]
switchyard check [--config f]
switchyard version
```

`pick` prints the label on stdout and exits 0, also on errors. The one
exception is `on_exhausted: fail` when everything is used up (with `--jobs`:
when no listed job has a runner): it then exits 3 and the action fails the
step. Under Actions it appends `runner`, `provider`, `paid`, `fallback`,
`runner_<size>` and `runners` to `$GITHUB_OUTPUT`. Logs and warnings
go to stderr.

Environment variables: `SWITCHYARD_TOKEN`, `SWITCHYARD_RUNNERS_TOKEN`, `SWITCHYARD_CONFIG`,
`SWITCHYARD_CACHE_FILE`, `SWITCHYARD_SIZE`, `SWITCHYARD_ARCH`,
`SWITCHYARD_FALLBACK`, `SWITCHYARD_DISABLE`, `SWITCHYARD_JOBS`, and the standard
`GITHUB_REPOSITORY`, `GITHUB_API_URL`, `GITHUB_RUN_ID` and `RUNNER_NAME`.

```
$ switchyard usage
    PROVIDER  JOBS  MINUTES  WEIGHTED  BILLED  FREE  LEFT    CAP  EST. COST       STATUS
          pi    40      210       210       -     -     -      -          -  self-hosted
         jog   120     1830      2410       -  5000  2590      -      $0.00         free
  blacksmith     0        0         0       -  3000  3000      -      $0.00         free
      github   300      950       950     990  2000  1010  $5.00      $0.00         free
     (other)    12       30        30       -     -     -      -          -  not counted
```

## Development and releases

```
go vet ./... && go test ./...
go run ./cmd/switchyard check
```

To release, push a tag like `v1.0.0`. The release workflow runs GoReleaser.
It builds `linux/amd64`, `linux/arm64` and `darwin/arm64`, attaches the
archives (`switchyard_<os>_<arch>.tar.gz`, the names `action.yml` expects) and
moves the `v1` tag to the new release.

The action downloads the binary once per version into the runner's tool cache.
It checks the archive against the release's `checksums.txt` and does not run
it if they differ (the pick then falls back). It needs `jq` or `python3` on
the runner to read the release list.

`@v1` follows the newest 1.x.y release, because the `v1` tag is moved on each
release. To pin an exact version, use `@v1.2.3`, or a commit SHA that has a
`vX.Y.Z` tag: the action then downloads that release's binary.

To build from source instead: `go install github.com/isromero/switchyard/cmd/switchyard@latest`.
