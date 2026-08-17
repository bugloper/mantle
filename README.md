# Mantle

**The registry that knows what you deployed.**

Mantle is a self-hostable OCI Distribution registry that links every image to the
commit that produced it and the servers running it — and that will not let a
retention policy delete an image production is running.

The registry protocol is unremarkable by design. Docker, Podman, BuildKit,
containerd, Skopeo and Cosign work against it with no configuration beyond a
hostname and credentials. The product is the layer above: a **deployment
ledger**, and retention that is deployment-aware by construction.

```
$ mantle ledger status --repo acme/api

acme/api                                            production · 2 host(s)
──────────────────────────────────────────────────────────────────────
NOW RUNNING     v2.4.1     64679f025250   commit a3f81c2   deployed 3h ago
                by nima · 2/2 hosts confirmed · reported via ansible
                web-1, web-2

ROLLBACK TO     v2.4.0     71bd9c1e4a02   commit 8e9d114   pinned ✓

TAGS            v2.4.1  v2.4.0  main-20260814
STORAGE         12.4 GiB across 38 manifests · GC would free ~3.2 GiB
SOURCE          https://github.com/acme/api
```

---

## What this build is

This repository implements the spine of the specification: a registry that
genuinely works end to end, with the differentiating ledger layer on top.
[`docs/STATUS.md`](docs/STATUS.md) states precisely what is implemented, what is
partial, and what is not built — read it before relying on anything.

In short: **M1 and most of M2 are here, plus the M3 ledger**, and a read-only
web interface (`mantle-ui`). The S3 driver, ACME, webhook delivery, replication,
scanning, and OIDC are not.

## Install the CLI

```bash
curl -fsSL https://raw.githubusercontent.com/bugloper/mantle/main/contrib/install.sh | sh
```

Detects your platform, fetches the `mantle` binary from GitHub Releases, checks
it against the published SHA-256, and installs it somewhere already on your
PATH — no privilege escalation unless you point it somewhere that needs it.
`MANTLE_VERSION` pins a version and `MANTLE_INSTALL_DIR` chooses the location.

That pattern runs whatever the server returns, so if you would rather read it
first — and [`contrib/install.sh`](contrib/install.sh) is written to be read:

```bash
curl -fsSLO https://raw.githubusercontent.com/bugloper/mantle/main/contrib/install.sh
less install.sh && sh install.sh
```

This installs the CLI only. The registry daemon needs a database and a
configuration file, so it ships as a container image — see below.

## Quick start

Two ways in. Docker needs nothing else installed; the source path gives you the
development loop.

### With Docker

```bash
docker compose up --build -d
docker compose run --rm -e MANTLE_ADMIN_PASSWORD='choose-something' mantled --bootstrap
```

That builds two images from the one [`Dockerfile`](Dockerfile) — `mantled` and
the optional `mantle-ui` — starts them alongside PostgreSQL 16, and creates the
first administrator.

To run published images instead of building from this checkout — which needs
nothing from this repository — [`docs/docker.md`](docs/docker.md) has a compose
file that pulls `bugloper/mantle` and `bugloper/mantle-ui`, plus TLS, upgrade
and backup guidance. Both images are built for `linux/amd64` and `linux/arm64`.

Either way you get:

```
  registry   http://localhost:5000
  interface  http://localhost:5180   (admin / choose-something)
```

The registry is configured entirely through `MANTLE_*` environment variables in
[`docker-compose.yml`](docker-compose.yml); there is no config file in the
image, which is why `--bootstrap` above passes no `--config`. Metrics are served
on `:9090` inside the container and deliberately not published.

Two things to know before you change the port. `server.domain` is what the
token flow advertises to clients, so a different published port means changing
`MANTLE_SERVER_DOMAIN` to match, or the Docker login dance will send clients
back to a port nothing is listening on. And on macOS, ControlCenter's AirPlay
Receiver already holds `:5000` and will answer instead of the registry — which
is the whole reason the source path below uses `5100`.

This stack runs over plain HTTP on localhost. That is fine for evaluation and
wrong for anything else.

### From source

You need Go 1.26+ and PostgreSQL 15+ running. Then one command:

```bash
make up
```

That builds all three binaries, creates a local database, bootstraps an
administrator, and starts the registry and the web interface in the background:

```
  registry   http://127.0.0.1:5100
  interface  http://127.0.0.1:5180   (admin / devpassword)
  metrics    http://127.0.0.1:9190/metrics
```

`make dev-seed` pushes a few images with provenance labels and records
deployments against them, so there is something to look at. `make status`,
`make logs` and `make down` do what they say, `make up` is safe to re-run, and
`make dev-clean` drops the database and the state directory when you are done.

<details>
<summary>Running it for real, rather than locally</summary>

```bash
make build
createdb mantle
cp docs/mantle.example.yaml /etc/mantle/mantle.yaml
$EDITOR /etc/mantle/mantle.yaml     # set domain, database.url, storage root, TLS

mantled --config /etc/mantle/mantle.yaml --check   # validate before starting

# Create the first administrator. Idempotent — it will not replace an
# existing one.
MANTLE_ADMIN_PASSWORD='choose-something' mantled --config /etc/mantle/mantle.yaml --bootstrap

mantled --config /etc/mantle/mantle.yaml
```

`--bootstrap-username` and `--bootstrap-org` override the account and default
organization `--bootstrap` creates. There is no `mantle install` yet, and no
packaging: no service unit, no `.deb`/`.rpm`, no published image. Migrations
are embedded and applied on startup under an advisory lock, so starting the
daemon is all that is needed to bring a schema up to date.
</details>

Then, from anywhere:

```bash
mantle login http://127.0.0.1:5100 --username admin --token 'choose-something'
mantle org create acme
mantle setup --repo acme/api        # creates tokens, prints the snippets
```

`mantle setup` prints a builder token, a pull token, the provenance labels to add
to your build, and the deploy-reporting snippet for your deploy tool.

`mantle login` stores credentials in `~/.config/mantle/credentials.yaml`;
`MANTLE_CLI_CONFIG` points it somewhere else, which is how the seed script keeps
out of your real profile. `mantle doctor` checks a live instance — the
database, pending migrations, storage, and the garbage collector's sweep — and
names the next action for every failing check. It reads `/readyz`, which answers
without credentials, so it still works when the credential is the problem.

### The web interface

```bash
mantle-ui --registry http://127.0.0.1:5100      # then open http://127.0.0.1:5180
```

`mantle-ui` is a **separate, optional** binary (§14.3). It holds no database
connection and no privileged access — it is an ordinary client of the same
`/api/v1` the CLI uses, and it proxies your browser's credential through rather
than holding one of its own. The registry runs perfectly well without it.

It shows the instance overview, repositories, the deployment ledger,
organizations, identities and storage, and it can create organizations,
repositories, users and tokens; change visibility and tag immutability; revoke
credentials; delete repositories; and run garbage collection.

Every one of those is authorised by the registry against your own credential,
not by the interface. What the interface decides is only what to *offer* — so a
reader sees fewer buttons, and a non-administrator sees a note instead of the
panels they cannot read.

```bash
mantle-ui --registry http://127.0.0.1:5100 --read-only
```

`--read-only` refuses every state-changing request at the proxy, turning the
interface back into a dashboard with the CLI as the only way to make changes.
That is the posture §14.3 recommends, and it is worth considering for anything
reachable beyond your own machine.

Assets are embedded in the binary: no Node runtime, no build step.

## The two things that make it different

### 1. Provenance with zero integration

Push an image built by any modern builder and Mantle reads the commit from the
standard OCI labels the builder already wrote:

```bash
docker buildx build \
  --label org.opencontainers.image.revision="$(git rev-parse HEAD)" \
  --label org.opencontainers.image.source="$(git remote get-url origin)" \
  -t registry.example.com/acme/api:v2.4.1 --push .
```

No hook, no agent, no plugin. Where labels are absent, Mantle falls back to
tag-shape inference and records that the result was a *guess* — so a wrong
inference is diagnosable rather than mysterious.

### 2. Retention that cannot delete what is running

One HTTP call from whatever already deploys upgrades the record to *reported*:

```bash
mantle deploy record --repo acme/api --digest "$DIGEST" \
  --env production --host "$(hostname)" --status active || true
```

That image is now **pinned**. It is a root in the garbage collector's reachable
set and excluded from every retention policy, and the database itself refuses to
delete a manifest a deployment references — so the guarantee survives a bug in
the collector, not merely a correct implementation of it.

The `|| true` is not a suggestion. Recording a deploy must never be able to fail
a deploy, and every shipped snippet is failure-tolerant.

`mantle setup --pack` prints this for Compose, systemd, Ansible, CI or curl.
[`docs/kamal.md`](docs/kamal.md) works the same ground for
[Kamal](https://kamal-deploy.org) end to end — registry credentials, getting the
commit out of a Kamal build, and a `post-deploy` hook that reports every host in
one local call.

## Architecture

```
mantled (one Go binary)          PostgreSQL          filesystem
  :443   /v2/*    OCI API         metadata            content-addressed
         /auth/*  tokens          manifests           blobs only
         /api/v1  admin API       ledger
  :9090  metrics, health          audit
  in-process workers (leader-elected via advisory lock):
    gc · ledger · partition maintenance
```

Three design decisions are load-bearing and worth knowing about:

**Manifests live in Postgres, not the blob store.** They are small, extremely
hot, and must be byte-exact. Serving them from a row removes a storage
round-trip from every pull and eliminates the "manifest exists in the database
but not in storage" class of inconsistency. The cost is that `pg_dump` becomes
load-bearing for image retrieval.

**Garbage collection is online and two-phase.** A grace window, transactional
edges declared `ON DELETE RESTRICT`, and a quarantine period each independently
prevent the classic upload-versus-collect race. Deletion is recoverable for a
week by flipping a column.

**Nothing imports the distribution package.** The pull path is the only path
where an outage is a production incident, so no product feature may become a
dependency of it. The ledger is reached through a narrow `events.Sink`
interface, and the rule is enforced by a test in `test/architecture`.

## Configuration

Resolution order is **flags > `MANTLE_*` environment > file > defaults**, and
every default is chosen to be safe when left alone. The full annotated surface
is [`docs/mantle.example.yaml`](docs/mantle.example.yaml);
[`docs/mantle.dev.yaml`](docs/mantle.dev.yaml) is the loopback-only template
`make dev-setup` fills in.

`mantled` reads `/etc/mantle/mantle.yaml` unless `--config` or `MANTLE_CONFIG`
says otherwise. A missing file at the *default* path is not an error — running
entirely from the environment is a supported deployment — but a missing file at
a path you named explicitly is. Every environment variable mirrors a YAML key:
`storage.filesystem.root` is `MANTLE_STORAGE_FILESYSTEM_ROOT`.

Validation names the key, the value it got, and the range it accepts, and
`mantled --check` runs it without starting anything. Options that exist in the
schema but have no implementation behind them — `storage.driver: s3`,
`server.tls.mode: acme`, `auth.oidc` — fail loudly at startup rather than
appearing to work.

## Development

```bash
make help           # every target, with a one-line description

make up             # build, bootstrap, and start everything in the background
make down           # stop it
make status         # is it healthy?
make logs           # follow both logs
make dev-seed       # push sample images and deployments
make dev-clean      # stop, drop the database, remove the state directory

make build          # all three binaries into ./bin
make mantled        # or just one of them — also mantle, mantle-ui
make clean          # remove ./bin

make test           # unit tests (no database needed)
make test-all       # everything, against a real PostgreSQL
make test-race      # everything, under the race detector
make lint           # gofmt + go vet + the dependency rule
make tidy           # go mod tidy

make dev-setup      # create the local database and bootstrap an admin
make dev            # run the registry in the foreground
make dev-ui         # run the web interface in the foreground

make release-dry-run  # build the images for every architecture, push nothing
make release          # build and publish to Docker Hub
```

`make release` runs [`contrib/release.sh`](contrib/release.sh), which publishes
two things: the container images to Docker Hub, and a GitHub Release carrying
the CLI binaries for macOS and Linux on amd64 and arm64 — which is what
`contrib/install.sh` fetches. `--skip-images` and `--skip-binaries` do one
without the other.

It refuses a dirty tree or an untagged commit, asks for confirmation before
publishing anything public, and afterwards checks that the image reports the
version it claims, that the manifest carries every architecture, and that a
freshly built CLI archive actually runs. `docs/docker.md` describes it in full.

Binaries build to a temporary name and are moved into place, so rebuilding
while a daemon is running is safe. Overwriting a running executable in place
invalidates its code signature and macOS kills the process outright, which is a
confusing way to lose a registry mid-session.

The interface can be iterated on without rebuilding: `mantle-ui --assets
cmd/mantle-ui/assets` serves from disk instead of the embedded copy.

Tests run against a real PostgreSQL rather than a mock, because Mantle's
correctness claims are largely claims about transactions and constraints. Point
them at a server with:

```bash
export MANTLE_TEST_DATABASE_URL="postgres://$USER@localhost/postgres?sslmode=disable"
```

Each test gets a throwaway database cloned from a migrated template, so the
suite is fast and tests cannot interfere with one another. Tests needing a
database skip themselves when that variable points at nothing reachable, which
is why `make test` works on a machine with no PostgreSQL and `make test-all` is
the honest run.

## Layout

```
cmd/mantled            the daemon
cmd/mantle             the CLI — every command is an API call
cmd/mantle-ui          the web interface — separate, optional, embedded assets

internal/oci           digests, names, manifests (leaf package)
internal/distribution  the /v2 HTTP surface, and nothing else
internal/catalog       repositories, blobs, manifests, tags (Postgres)
internal/storage       the blob driver interface and the filesystem backend
internal/auth          identities, tokens, RBAC
internal/ledger        the deployment ledger — the differentiator
internal/gc            online garbage collection
internal/events        the narrow sink the pull path publishes through
internal/admin         the /api/v1 surface
internal/server        assembly, middleware, background workers
internal/config        the configuration surface and its validation
internal/migrate       embedded migrations, checksum-verified
internal/observability metrics, structured logs, credential scrubbing
internal/testsupport   throwaway databases and fixtures

test/integration       end-to-end, over real HTTP against real Postgres
test/architecture      the dependency rule, enforced

docs/                  status, config templates, Docker and Kamal guides
contrib/               install.sh, dev-seed.sh, release.sh
```

## Licence

Apache-2.0 (D-03). The deployment ledger stays in the free core: it is the
reason to adopt Mantle, and putting the differentiator behind a paywall would
mean nobody discovers it.
