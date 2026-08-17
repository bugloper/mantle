# Deploying with Kamal

[Kamal](https://kamal-deploy.org) builds an image, pushes it to a registry, and
starts it on your servers over SSH. It already knows every fact Mantle's ledger
wants — the commit, the version, the hosts, who ran it, and whether it was a
deploy or a rollback — and it will hand all of them to a shell script for free.
Connecting the two is a `registry:` block and one hook.

Checked against **Kamal 2.12**. Where a detail depends on Kamal's internals
rather than its documented interface, this file says so.

---

## 1. Point Kamal at Mantle

`mantle setup` creates the tokens and prints them once:

```bash
mantle setup --repo acme/api
```

In `config/deploy.yml`:

```yaml
service: api
image: acme/api

registry:
  server: registry.example.com
  username: kamal
  password:
    - KAMAL_REGISTRY_PASSWORD
```

And in `.kamal/secrets`:

```
KAMAL_REGISTRY_PASSWORD=mantle_dep_a1b2c3d4e5f60718_xxxxxxxxxxxxxxxxxxxxxxxxxxx
```

**The username is ignored.** Mantle's machine credentials carry their own
lookup key — a secret beginning `mantle_dep_` is split at the first `_` after
the prefix and resolved by its selector, so no username is consulted
(`internal/auth/identity/identity.go`, `Authenticate`). Docker still requires
the field, so put something you will recognise: a failed token request logs the
username it was given, which is the one place it shows up. This is only true of
machine tokens: a *user* account authenticates by name and password in the
ordinary way.

### Which token goes in `registry:`

`mantle setup` mints two — a `contributor` builder token and a `reader` token
for servers — on the assumption that the machine pushing and the machines
pulling hold different credentials. Kamal has one `registry:` block, and
`kamal registry login` runs **both locally and on every host**. So whichever
token you configure is distributed to your app servers.

That leaves a choice, and it is worth making deliberately rather than by
default:

- **Simple:** use the builder token. `kamal deploy` builds, pushes, and pulls
  with one credential. Every app server then holds a token that can push to
  `acme/api`.
- **Least privilege:** put the pull-only `reader` token in `registry:`, push
  from CI with the builder token, and deploy with `kamal deploy --skip-push`
  (`-P`), which pulls the existing image instead of building one. Your servers
  then hold a credential that cannot write to the registry.

The second is the right posture for anything with more than one operator, and
it costs one CI step.

---

## 2. Provenance — most of it is already free

Kamal tags images with the full 40-character git SHA (`Kamal::Git.revision`),
which is exactly the shape Mantle's tag inference recognises. Push with Kamal
and the ledger links the image to its commit with no configuration at all.

It is recorded as a **guess**, though, and the distinction matters. Verified
against `internal/ledger/provenance.go`:

| Tag Kamal produces | Commit Mantle records | Source | Confidence |
|---|---|---|---|
| `3f0a9c1e…4f6a` (clean tree) | `3f0a9c1e…4f6a` ✅ | `tag_pattern` | probable |
| `3f0a9c1e…4f6a_uncommitted_9d4e…` (dirty tree) | `9d4e2a1b3c5f7a80` ❌ | `tag_pattern` | probable |
| `v2.4.1` (explicit `--version`) | none | — | — |

The middle row is the one to know about. When the working tree is dirty and
`builder: git_clone` is not in use, Kamal appends `_uncommitted_<16 hex>` to the
version. Mantle's last-resort rule takes the trailing hex group of a
`<something>-<hex>` or `<something>_<hex>` tag, so it captures Kamal's
random uncommitted marker and records *that* as the commit. It resolves to
nothing in your history.

This is inference behaving as designed rather than a bug — the fact is stored as
`tag_pattern` / `probable` with the original tag kept in
`raw["inferred_from_tag"]`, so it is diagnosable rather than mysterious (§13.2).
But a wrong commit in front of someone mid-incident is exactly what the
confidence marking exists to prevent you from trusting, and the fix is cheap.

### Setting the labels properly

An annotation or label beats inference outright, and is recorded as `certain`.
Kamal's builder has no `labels:` key (checked in
`lib/kamal/configuration/builder.rb`, 2.12 — it exposes `args`, `secrets`,
`dockerfile`, `target`, `context`, and others, but not labels), so the way in is
a build arg the Dockerfile turns into a label.

In your `Dockerfile`:

```dockerfile
ARG GIT_SHA
ARG GIT_SOURCE
LABEL org.opencontainers.image.revision=$GIT_SHA
LABEL org.opencontainers.image.source=$GIT_SOURCE
```

And in `config/deploy.yml`, which Kamal evaluates as ERB:

```yaml
builder:
  args:
    GIT_SHA: <%= `git rev-parse HEAD`.strip %>
    GIT_SOURCE: <%= `git remote get-url origin`.strip %>
```

Now the commit is a statement the builder made, the dirty-tree trap disappears,
and `mantle ledger status` shows a commit it can stand behind.

> **Not to be confused with** `builder: provenance:` in Kamal, which controls
> BuildKit's SLSA provenance attestation. That is a different artifact for a
> different purpose; Mantle's Tier 0 provenance reads OCI annotations and image
> config labels.

---

## 3. Record the deployment

Provenance says where an image came from. It does not say that anything is
*running* it — and running is what pins an image against garbage collection and
retention (§13.4). That takes one call, from Kamal's `post-deploy` hook.

`.kamal/hooks/post-deploy`:

```sh
#!/usr/bin/env sh
#
# Record this deploy in Mantle's ledger, which pins the image against GC and
# retention for as long as it is running.
#
# Reporting must never be able to fail a deploy, so every path here exits 0.

REPO=acme/api
export MANTLE_REGISTRY=https://registry.example.com
export MANTLE_TOKEN="${KAMAL_REGISTRY_PASSWORD:-}"

# post-deploy fires for rollback too.
case "${KAMAL_COMMAND:-}" in
  rollback) STATUS=rolled_back ;;
  *)        STATUS=active ;;
esac

# KAMAL_HOSTS is comma-separated; --host is repeatable.
HOSTS=""
for host in $(echo "${KAMAL_HOSTS:-}" | tr ',' ' '); do
  HOSTS="$HOSTS --host $host"
done

mantle deploy record \
  --repo "$REPO" \
  --tag "${KAMAL_VERSION:-}" \
  --env "${KAMAL_DESTINATION:-production}" \
  --status "$STATUS" \
  --performer "${KAMAL_PERFORMER:-}" \
  --tool kamal \
  --deploy-id "${KAMAL_VERSION:-}-${KAMAL_RECORDED_AT:-}" \
  $HOSTS || true

exit 0
```

Make it executable: `chmod +x .kamal/hooks/post-deploy`.

Several things about that script are load-bearing:

**`--tag`, not `--digest`.** Kamal gives you a version, not a digest, and
Mantle resolves the tag to a manifest server-side. There is nothing to look up.

**No `--commit`.** When the caller omits one, Mantle fills it from the
provenance already stored against that manifest — the image knows its own commit
and the hook does not have to.

**The credential is the one already there.** `post-deploy` runs with
`secrets: true`, so Kamal's secrets are in the hook's environment, and
`KAMAL_REGISTRY_PASSWORD` is reused as `MANTLE_TOKEN`. No second credential to
provision or rotate. Reporting a deployment requires only *read* on the
repository (§13.2), so a pull-only token is enough — which means this works
under the least-privilege setup above too.

**`--deploy-id` makes it idempotent.** Repeats with the same id collapse to one
record, so a re-run of the same deploy does not produce a second entry.
`KAMAL_RECORDED_AT` is stamped once per invocation, so distinct deploys of the
same version still record separately.

**`|| true` and `exit 0` are not decoration.** A hook that exits non-zero fails
the Kamal run. The deploy has already happened by the time `post-deploy` fires,
so letting the ledger fail it would be strictly worse than not reporting at all
(REQ-LEDGER-02).

**The hook runs once, locally.** Kamal hooks execute on the machine invoking
`kamal`, not on the app servers — `KAMAL_HOSTS` is how one local call reports
every host. Mantle needs no agent on your servers.

### What Kamal passes to `post-deploy`

| Variable | Contents |
|---|---|
| `KAMAL_VERSION` | the image tag — git SHA, or `<sha>_uncommitted_<hex>` |
| `KAMAL_HOSTS` | comma-separated hosts deployed to |
| `KAMAL_PERFORMER` | `git config user.email`, falling back to `whoami` |
| `KAMAL_RECORDED_AT` | ISO8601 UTC, stamped once per run |
| `KAMAL_DESTINATION` | the `-d` destination, if set |
| `KAMAL_COMMAND` | `deploy`, `redeploy`, or `rollback` |
| `KAMAL_ROLES` | comma-separated roles, if narrowed |
| `KAMAL_RUNTIME` | seconds the deploy took |

`KAMAL_COMMAND` and `KAMAL_ROLES` are assembled in `run_hook`
(`lib/kamal/cli/base.rb`) and are not listed in the sample hook Kamal generates,
but they are present.

### Richer records

`mantle deploy record` covers the common case. The API accepts two fields the
CLI has no flag for — `deploy_tool_version` and a free-form `metadata` object —
so if you want `KAMAL_RUNTIME` and the roles on the record, post directly:

```sh
curl -sf -m 5 -X POST "$MANTLE_REGISTRY/api/v1/deployments" \
  -H "Authorization: Bearer $MANTLE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"repository\":\"$REPO\",
       \"tag\":\"$KAMAL_VERSION\",
       \"environment\":\"${KAMAL_DESTINATION:-production}\",
       \"status\":\"$STATUS\",
       \"deploy_tool\":\"kamal\",
       \"deploy_tool_version\":\"2.12.0\",
       \"metadata\":{\"runtime_seconds\":\"${KAMAL_RUNTIME:-}\",
                     \"roles\":\"${KAMAL_ROLES:-}\"}}" || true
```

---

## 4. Rollbacks

`kamal rollback` fires `post-deploy` with `KAMAL_COMMAND=rollback`, which the
hook above turns into `--status rolled_back`. The ledger then holds both the
version that was rolled back and the one now running, which is what
`mantle ledger status` needs to print a `ROLLBACK TO` line that is true.

One limit is worth being precise about, because it is easy to assume Mantle
covers more of it than it does. `kamal rollback <version>` checks that a
**container** for that version still exists on every app host
(`container_available?`) and refuses otherwise. It never consults the registry,
so Mantle's pinning does nothing for that command — and `kamal deploy` prunes
old containers at the end of every run, so the window is however many your
`retain_containers` setting keeps.

Past that window the registry is what you have left:

```bash
kamal deploy --version=<sha> --skip-push
```

`--version` is a global option and `--skip-push` pulls the existing image
instead of building one, so this boots a specific old version straight from
Mantle. *That* is where pinning earns its place — the pull succeeds because a
deployed image cannot be removed by retention or GC, however long ago it was
superseded. The ledger's `ROLLBACK TO` line names a version this command can
still reach.

---

## 5. Check it worked

```bash
mantle ledger status --repo acme/api
```

You should see the version Kamal deployed under `NOW RUNNING`, the hosts from
`KAMAL_HOSTS` confirmed, `reported via kamal`, and the commit — as a fact if you
set the labels in §2, as a probable inference if you left it to the tag shape.

If the commit looks wrong, check whether the tag carries an `_uncommitted_`
suffix before suspecting anything else.

---

## Caveats

- **There is no `mantle setup --pack kamal`.** The packs are `compose`,
  `systemd`, `ansible`, `ci`, and `curl`; the hook above is not generated for
  you. Copy it from here.
- **None of this is covered by an integration test.** The Kamal-side facts were
  read from Kamal 2.12's source and the Mantle-side behaviour was checked
  against `internal/ledger/provenance.go` and `internal/admin/ledger.go`, but no
  test in this repository drives a real `kamal deploy` against a real Mantle. It
  belongs in the client matrix gap [`STATUS.md`](STATUS.md) already names.
- **Accessories are not recorded.** `kamal accessory boot` pulls images Mantle
  will happily serve, but the hook above reports only the main service. An
  accessory image is therefore not pinned by anything and remains subject to
  retention.
