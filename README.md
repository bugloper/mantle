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

## Quick start

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
`make logs` and `make down` do what they say, and `make up` is safe to re-run.

<details>
<summary>Running it for real, rather than locally</summary>

```bash
make build
createdb mantle
cp docs/mantle.example.yaml /etc/mantle/mantle.yaml
$EDITOR /etc/mantle/mantle.yaml     # set domain, database.url, storage root, TLS

# Create the first administrator. Idempotent — it will not replace an
# existing one.
MANTLE_ADMIN_PASSWORD='choose-something' mantled --config /etc/mantle/mantle.yaml --bootstrap

mantled --config /etc/mantle/mantle.yaml
```
</details>

Then, from anywhere:

```bash
mantle login http://127.0.0.1:5100 --username admin --token 'choose-something'
mantle org create acme
mantle setup --repo acme/api        # creates tokens, prints the snippets
```

`mantle setup` prints a builder token, a pull token, the provenance labels to add
to your build, and the deploy-reporting snippet for your deploy tool.

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

## Development

```bash
make up             # build, bootstrap, and start everything in the background
make down           # stop it
make status         # is it healthy?
make logs           # follow both logs
make dev-seed       # push sample images and deployments

make build          # all three binaries into ./bin
make mantled         # or just one of them
make test           # unit tests (no database needed)
make test-all       # everything, against a real PostgreSQL
make lint           # gofmt + go vet + the dependency rule
make dev            # run the registry in the foreground
make dev-ui         # run the web interface in the foreground
```

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
suite is fast and tests cannot interfere with one another.

## Layout

```
cmd/mantled            the daemon
cmd/mantle             the CLI — every command is an API call
cmd/mantle-ui          the web interface — separate, optional, read-only
internal/oci          digests, names, manifests (leaf package)
internal/distribution the /v2 HTTP surface, and nothing else
internal/catalog      repositories, blobs, manifests, tags (Postgres)
internal/storage      the blob driver interface and the filesystem backend
internal/auth         identities, tokens, RBAC
internal/ledger       the deployment ledger — the differentiator
internal/gc           online garbage collection
internal/admin        the /api/v1 surface
internal/server       assembly, middleware, background workers
test/integration      end-to-end, over real HTTP against real Postgres
test/architecture     the dependency rule, enforced
```

## Licence

Apache-2.0 (D-03). The deployment ledger stays in the free core: it is the
reason to adopt Mantle, and putting the differentiator behind a paywall would
mean nobody discovers it.
