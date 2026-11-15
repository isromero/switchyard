# switchyard

Picks the runner for each GitHub Actions run: your own machines when they are
idle, otherwise a cloud provider, without paying while free minutes are left.

- **Your machines first, never waiting on them.** `runs-on: my-server` alone
  waits for hours if the machine is busy or off. switchyard uses it only when
  it's idle, and sends the job elsewhere otherwise.
- **Failover between providers.** If a provider stops starting jobs (app not
  set up, out of credit, outage), the next runs go to the next one.
- **Spend limits across providers.** Use free minutes first, then paid
  providers up to a monthly cap, then stop or fall back, as you choose.
- **A runner per job.** Spread a run's jobs over your idle machines and the
  cloud.
- **Safe for public repos.** Pull requests from forks never run on your
  machines.

It's one step at the start of a workflow. No server, no database, no account.

## Quick start

```yaml
jobs:
  pick:
    runs-on: ubuntu-latest
    outputs:
      runner: ${{ steps.pick.outputs.runner }}
    steps:
      - uses: actions/checkout@v4
      - id: pick
        uses: isromero/switchyard@v1
        with:
          token: ${{ secrets.SWITCHYARD_TOKEN }}

  test:
    needs: pick
    runs-on: ${{ needs.pick.outputs.runner }}
    steps:
      - uses: actions/checkout@v4
      - run: make test
```

Then add `.github/switchyard.yml`. Providers are tried in order:

```yaml
fallback: ubuntu-latest

providers:
  - name: my-server
    kind: self-hosted
    runners: [{ label: my-server }]

  - name: blacksmith
    free_minutes: 3000
    runners: [{ label: blacksmith-2vcpu-ubuntu-2404 }]

  - name: github
    free_minutes: 2000
    ignore_public_repos: true
    runners: [{ label: ubuntu-latest }]
```

[`switchyard.yml`](switchyard.yml) is a full example with the labels of
several providers. Run `switchyard check` to validate a config and see which
token permissions it needs.

## Token

The built-in `GITHUB_TOKEN` only sees the current repository, so create a
fine-grained token and save it as the `SWITCHYARD_TOKEN` secret:

- **Actions: read** on the repositories whose minutes should count.
- **Administration: read** on the repositories that use self-hosted runners,
  to see which are idle. You can put this in a separate `runners-token`.

## One runner per job

```yaml
      - id: pick
        uses: isromero/switchyard@v1
        with:
          token: ${{ secrets.SWITCHYARD_TOKEN }}
          jobs: lint, test*3          # test*3 = test-1, test-2, test-3

  lint:
    runs-on: ${{ fromJSON(needs.pick.outputs.runners).lint }}
  test:
    strategy: { matrix: { shard: [1, 2, 3] } }
    runs-on: ${{ fromJSON(needs.pick.outputs.runners)[format('test-{0}', matrix.shard)] }}
```

Idle machines of yours take the first jobs, one each; the rest go to the
cloud.

## Inputs and outputs

| Input | |
|---|---|
| `token` | Required. See [Token](#token). |
| `runners-token` | Optional token used only to list self-hosted runners. |
| `config` | Config path. Default: `.github/switchyard.yml`. |
| `size` | `small` (default), `medium` or `large`. |
| `arch` | `x64`, `arm64` or `any` (default). |
| `jobs` | Jobs that each get a runner, e.g. `lint, test:medium*3`. |
| `disable` | Providers to skip this time, e.g. `blacksmith`. |
| `fallback` | Label used when nothing fits or on any error. Default: the config's `fallback`. |

| Output | |
|---|---|
| `runner` | The label to use. |
| `runners` | JSON with a label per job in `jobs`. |
| `runner_small`, `runner_medium`, `runner_large` | A label per size. |
| `provider` | The provider picked; empty for the fallback. |
| `paid` | `true` if this run will be billed. |

switchyard never breaks a workflow: on any error it outputs the fallback and
logs a warning.

## CLI

```
switchyard usage    # minutes used per provider this month
switchyard pick     # print the label it would pick
switchyard check    # validate the config
```

Install with `go install github.com/isromero/switchyard/cmd/switchyard@latest`
or download a binary from the releases.

## Roadmap

- [x] Idle self-hosted runners first, cloud otherwise
- [x] Free minutes first, then paid providers up to a monthly cap
- [x] Skip providers that stop starting jobs
- [x] A runner per job, for matrices
- [x] Pull requests from forks never use your machines
- [x] Minutes from GitHub's billing report
- [ ] Limit jobs at once per provider, sending the rest to the next one
- [ ] Self-hosted runners started on demand (ephemeral, scale sets)
- [ ] Pick when each job starts, not when the run starts

## More

[docs/reference.md](docs/reference.md) explains how a pick is made, how
minutes are counted, every config option and the CLI in detail.

## License

MIT
