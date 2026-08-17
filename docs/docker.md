# Self-hosting with Docker

Mantle is one static binary and a PostgreSQL database. Running it under Docker
needs a compose file and one bootstrap command, and nothing from this
repository — everything below can be copied into an empty directory.

```
bugloper/mantle      the registry daemon
bugloper/mantle-ui   the web interface — separate, and optional
```

Both are published for `linux/amd64` and `linux/arm64` as a manifest list, so
`docker pull` resolves the right one on a Graviton instance, a Raspberry Pi, or
a laptop without anybody picking a tag by architecture.

> **Before the first tagged release**, these images do not exist on Docker Hub
> yet — they are published by [`contrib/release.sh`](../contrib/release.sh).
> Until then, clone the repository and use its `docker-compose.yml`, which
> builds the same two images locally.

---

## The quickest thing that works

`docker-compose.yml`:

```yaml
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: mantle
      POSTGRES_PASSWORD: change-me
      POSTGRES_DB: mantle
    volumes:
      - postgres-data:/var/lib/postgresql/data
    healthcheck:
      # mantled will not start against an unreachable database, so it waits
      # for this rather than crash-looping.
      test: ["CMD-SHELL", "pg_isready -U mantle"]
      interval: 2s
      timeout: 3s
      retries: 15

  mantled:
    image: bugloper/mantle:latest
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "5000:5000"
    environment:
      MANTLE_SERVER_DOMAIN: registry.example.com
      MANTLE_SERVER_LISTEN: "0.0.0.0:5000"
      MANTLE_SERVER_TLS_MODE: "off"
      MANTLE_DATABASE_URL: postgres://mantle:change-me@postgres/mantle?sslmode=disable
      MANTLE_STORAGE_FILESYSTEM_ROOT: /var/lib/mantle
      MANTLE_AUTH_SIGNING_KEY_PATH: /var/lib/mantle/keys/token.pem
      MANTLE_OBSERVABILITY_METRICS_LISTEN: "0.0.0.0:9090"
    volumes:
      - mantle-data:/var/lib/mantle
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://127.0.0.1:9090/readyz || exit 1"]
      interval: 5s
      timeout: 3s
      retries: 10

  # Optional. Delete this service and the registry is unaffected (§14.3).
  mantle-ui:
    image: bugloper/mantle-ui:latest
    depends_on:
      mantled:
        condition: service_healthy
    ports:
      - "5180:5180"
    command: ["--registry", "http://mantled:5000", "--listen", "0.0.0.0:5180"]

volumes:
  postgres-data:
  mantle-data:
```

Start it, then create the first administrator:

```bash
docker compose up -d
docker compose run --rm -e MANTLE_ADMIN_PASSWORD='choose-something' mantled --bootstrap
```

`--bootstrap` is idempotent: it will not replace an administrator that already
exists, so re-running it is safe.

There is **no configuration file in the image**. Everything is set through
`MANTLE_*` variables, which is why `--bootstrap` above passes no `--config` —
naming a path that does not exist is a fatal error by design.

---

## Two settings that matter more than they look

**`MANTLE_SERVER_DOMAIN` must be the address clients actually use.** It derives
the token realm advertised in the `WWW-Authenticate` challenge, which is where
`docker login` is sent to collect a token. Set it to `localhost:5000` while the
published port is `5001` and the login dance sends clients to a port nothing is
listening on, with an error that does not mention the domain.

**Pick a port that is free.** On macOS, ControlCenter's AirPlay Receiver holds
`:5000` and will answer instead of the registry. Change the published port and
`MANTLE_SERVER_DOMAIN` together, never one alone.

---

## TLS

`MANTLE_SERVER_TLS_MODE: "off"` is what the compose file above uses, and it is
correct only behind something that terminates TLS, or on a machine nothing else
can reach. Docker clients refuse plain HTTP registries unless every client is
reconfigured to allow it, which is not a thing to ask of your users.

Two supported ways to have real TLS:

- **Terminate at a proxy** — Caddy, nginx, Traefik — and leave `TLS_MODE: off`
  behind it. Set `MANTLE_SERVER_DOMAIN` to the public name. This is the usual
  answer, and the proxy is also where you would put rate limiting, which Mantle
  does not implement.
- **Give mantled the certificate** with `MANTLE_SERVER_TLS_MODE: file`,
  `MANTLE_SERVER_TLS_CERT` and `MANTLE_SERVER_TLS_KEY`, mounting both into the
  container.

ACME is **not implemented** — `MANTLE_SERVER_TLS_MODE: acme` fails at startup
rather than pretending to work. Use a proxy if you want automatic certificates.

---

## Using it

```bash
docker login registry.example.com -u admin
mantle login https://registry.example.com --username admin --token 'choose-something'
mantle org create acme
mantle setup --repo acme/api
```

The registry image ships the `mantle` CLI alongside the daemon, so you can drive
it without installing anything locally:

```bash
docker compose exec mantled mantle --help
```

It will need credentials of its own — either `mantle login` inside the
container, or `MANTLE_REGISTRY` and `MANTLE_TOKEN` in the environment.

---

## Upgrading

```bash
docker compose pull
docker compose up -d
```

Migrations are embedded in the binary and applied at startup under an advisory
lock, so a new version brings its own schema and two nodes starting at once do
not race. Shutdown is graceful: `/readyz` starts failing first so a load
balancer drains the node, then in-flight requests are given time to finish.

Pin a version rather than tracking `latest` for anything you care about:

```yaml
image: bugloper/mantle:v0.1.0
```

`latest` moves on every stable release and never points at a pre-release.

The two images are versioned independently on purpose (§14.3) — upgrading the
registry never requires upgrading the interface, or the reverse.

---

## Backups

Two volumes hold everything, and both are needed:

- `postgres-data` — metadata, the ledger, **and every manifest**. Manifests
  live in Postgres rather than the blob store, so a `pg_dump` is load-bearing
  for image retrieval, not just for metadata. A restored blob store without its
  database serves nothing.
- `mantle-data` — content-addressed blobs, and the token signing key at
  `keys/token.pem`. Losing the key invalidates every issued token; clients
  recover by logging in again.

```bash
docker compose exec postgres pg_dump -U mantle mantle | gzip > mantle-$(date +%F).sql.gz
docker run --rm -v mantle-data:/data -v "$PWD:/backup" alpine \
  tar czf /backup/mantle-blobs-$(date +%F).tar.gz -C /data .
```

Take the database dump first. A dump older than the blob store is recoverable —
the extra blobs are unreferenced and garbage collection reclaims them. The
reverse is not: a database referencing blobs the store does not have is a
registry that serves broken images.

There is no `mantle backup` or `mantle export` command yet; see
[`STATUS.md`](STATUS.md).

---

## Publishing the images

Maintainers only. [`contrib/release.sh`](../contrib/release.sh) builds both
images for both architectures and pushes them to Docker Hub:

```bash
git tag -a v0.1.0 -m 'v0.1.0'
contrib/release.sh --dry-run     # build and verify, push nothing
contrib/release.sh v0.1.0        # publish
```

It refuses to release from a dirty tree or an untagged commit, rejects a
version that is not semantic, asks for confirmation before pushing anything
public, and verifies afterwards that the image reports the version it claims
and that the manifest carries every architecture. `--dry-run` does everything
except push.
