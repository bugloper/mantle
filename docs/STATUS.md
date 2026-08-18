# Implementation status

The specification describes roughly 36 engineering weeks of work across four
milestones. This document states exactly what this build does and does not
implement, so nothing here has to be discovered by trying it in production.

Legend: **✅ implemented and tested** · **◐ partial** · **❌ not built**

---

## §8 — OCI Distribution conformance

| ID | Endpoint | Status |
|---|---|---|
| end-1 | `GET /v2/` | ✅ |
| end-2 | `GET`/`HEAD /v2/<name>/blobs/<digest>` | ✅ with `Range` support |
| end-3 | `GET`/`HEAD /v2/<name>/manifests/<ref>` | ✅ byte-exact |
| end-4a | `POST /v2/<name>/blobs/uploads/` | ✅ |
| end-4b | `POST …/uploads/?digest=` (monolithic) | ✅ |
| end-5 | `PATCH …/uploads/<ref>` | ✅ incl. 416 on misaligned chunk |
| end-6 | `PUT …/uploads/<ref>?digest=` | ✅ |
| end-7 | `PUT /v2/<name>/manifests/<ref>` | ✅ |
| end-8a/8b | `GET /v2/<name>/tags/list` + pagination | ✅ keyset, `Link` header |
| end-9 | `DELETE /v2/<name>/manifests/<ref>` | ✅ tag and digest forms differ correctly |
| end-10 | `DELETE /v2/<name>/blobs/<digest>` | ✅ unlinks; GC reclaims |
| end-11 | Cross-repository mount | ✅ **incl. REQ-AUTHZ-01 source check** |
| end-12a/12b | Referrers API + `artifactType` filter | ◐ served from `subject_digest`; the fallback tag schema (`sha256-<hex>`) is **not** implemented |
| end-13 | `GET …/uploads/<ref>` | ✅ |
| — | `GET /v2/_catalog` | ✅ filtered by permission |

**Not run:** the upstream OCI conformance suite. The endpoints are implemented
against the specification and covered by integration tests, but the suite itself
is not wired into CI, so "conformant" is a claim this build has not earned.
That is M1's exit criterion and the single most valuable next step.

### Protocol requirements

| Requirement | Status |
|---|---|
| REQ-OCI-01 digest verification, constant-time | ✅ |
| REQ-OCI-02 manifest byte fidelity | ✅ tested end to end |
| REQ-OCI-03 `Docker-Content-Digest`, HEAD == GET | ✅ tested |
| REQ-OCI-04 media types | ✅ (schema 1 rejected with a named reason, D-05) |
| REQ-OCI-05 referential integrity, in-transaction | ✅ |
| REQ-OCI-06 name grammar | ✅ |
| REQ-OCI-07 `Range` semantics | ✅ tested |
| REQ-OCI-08 pagination | ✅ (`n=0` returns empty) |
| REQ-OCI-09 referrers | ◐ no fallback tag schema |
| REQ-OCI-10 error envelope | ✅ |
| REQ-OCI-11 existence disclosure | ✅ tested both halves |

---

## §9 — Identity and authorization

✅ Docker token flow in **both** shapes — `GET` with the scope in the query
string and a Basic header, and the OAuth2 `POST` form with `grant_type`,
`scope` and credentials in the body, which is what containerd and Docker 29
send. RS256 JWTs, JWKS at `/auth/jwks.json`, scope intersection, users / PATs /
robots / deploy tokens, Argon2id with a selector-verifier split, three-level
RBAC with explicit deny, REQ-AUTHZ-01, REQ-AUTHZ-02 (per-request
re-evaluation), anonymous pull for public repositories.

❌ OIDC (§9.4). The config field exists and is validated; there is no
implementation behind it. ❌ TOTP. ❌ `jti` revocation-before-expiry —
revocation works by disabling the identity, which takes effect immediately
because permissions are re-evaluated per request.

---

## §10–11 — Storage and data model

✅ Filesystem driver with fsync-before-rename durability, resumable uploads
across processes via checkpointed SHA-256 state (the M0 spike, kept as a
permanent test), global deduplication, the complete schema from §11 including
the ledger and audit tables.

❌ **The S3 driver.** The `Driver` interface accommodates it — presign,
multipart part accounting, spill state all have a place — but there is no
implementation. Configuring `storage.driver: s3` fails loudly at startup rather
than appearing to work. This is the largest single omission.

---

## §12 — Garbage collection

✅ All six phases: session cleanup, root set, transitive closure, mark,
unquarantine, sweep, and reconcile. Grace window and quarantine both enforced,
with the minimum grace period clamped in code regardless of configuration.
REQ-GC-01 through REQ-GC-05 hold. Dry run reports candidates with reasons.

The deployment-pinning guarantee is tested directly: an image that is deployed
survives a full collection cycle while an untagged, undeployed one is swept in
the same pass.

---

## §13 — The deployment ledger

✅ **Tier 0** — provenance from OCI annotations, image config labels, and
tag-shape inference, with the source and confidence of each fact recorded so a
guess is never presented as a fact.

✅ **Tier 1** — `POST /api/v1/deployments` and `mantle deploy record`, idempotent
by `external_id`, forgiving about missing fields, and never in the synchronous
path of a registry operation.

✅ Passive deployment inference from pull traffic, host tracking, the composed
ledger resource (§13.5), deployment-aware pinning (§13.4), and the
`mantle ledger status` view.

◐ Integration packs (§13.6): `mantle setup` generates the Compose, Kamal,
systemd, Ansible, CI and curl snippets, but they are generated text only — none
are shipped as files. `contrib/` exists and holds `dev-seed.sh`, a development
seeding script, not an integration pack.

✅ **Kamal** has a first-class integration outside this repository:
[`kamal-mantle`](https://github.com/bugloper/kamal-mantle) provisions Mantle
from the `docker-setup` hook, so a plain `kamal setup` stands the registry up
before it pushes, and reports every deploy to the ledger. It is a companion gem
rather than a Kamal patch — Kamal has no plugin API, but it does allow `x-`
extension keys at the document root and runs shell hooks, which is all this
needs.

❌ **Tier 2** — the host agent. Deliberately post-1.0 in the specification too.

---

## §14–15 — Operations and policy

| Feature | Status |
|---|---|
| Admin API `/api/v1/` | ◐ organizations, repositories (incl. create), users, tokens, grants, deployments, ledger, GC |
| CLI | ◐ see below |
| `mantle doctor` | ✅ against a live instance |
| Immutable tags, tag protection patterns | ✅ |
| Quotas | ✅ enforced at upload start and at commit; pulls never blocked |
| Retention policies | ❌ the schema and config exist; **no policy engine** |
| Rate limiting | ❌ not implemented |
| Webhooks | ◐ schema and delivery table only; **no dispatcher** |
| Pull-through cache | ❌ |
| Audit log | ◐ hash-chained append-only table with a database-level immutability trigger; **no writer wired into the handlers, and no `mantle audit verify`** |

**CLI implemented:** `version`, `login`, `doctor`, `org`, `user`, `token`,
`repo` (incl. `create`), `gc`, `ledger`, `deploy`, `setup`.

**CLI not implemented:** `install` (the full installer with preflight, ACME,
systemd unit and self-test), `uninstall`, `upgrade`, `config`, `backup`,
`export`/`import` (the §18.3 escape hatch), `retention`, `scan`.
`mantled --bootstrap` covers first-run administrator creation.

### The web interface (`mantle-ui`, §14.3)

Built, against the constraints §14.3 sets: a separate binary with its own
version, an ordinary API client with no database connection and no privileged
access, and optional at runtime. Assets are embedded, so no Node runtime is
required in production.

**Read:** ✅ overview, repositories list and detail, the deployment ledger view,
organizations, identities, storage and GC status. Panels needing an
administrator degrade to a note rather than an error.

**Write:** ✅ create organizations, repositories, users and tokens; change
visibility and tag immutability; revoke credentials; delete repositories; run a
GC dry run, a collection, or a reconcile. Destructive actions require a typed
confirmation. Generated secrets are shown once, in a dialog that says so.

Every write is authorised by the registry against the caller's own credential —
the proxy forwards and does not judge, so the two surfaces cannot disagree about
permissions. `--read-only` restores the dashboard-only posture the
specification recommends and enforces it at the proxy.

§14.3 recommends deferring writes until the read surface has been in real use.
That guidance was overridden deliberately; the mitigations are the typed
confirmations, the `--read-only` flag, and the fact that the CLI can do
everything the interface can (principle 7), verified by the architecture test.

❌ Not present: the audit log view (nothing writes audit records yet), image
detail with layers and referrers, retention policy screens, and grant
management beyond what token creation implies.

---

## §16 — Supply chain, observability, security

✅ Prometheus metrics, structured JSON logs with a central credential scrubber,
`/healthz` and `/readyz` with correct liveness/readiness semantics, graceful
drain on SIGTERM.

Security controls implemented: SEC-01 (paths derive from digests only),
SEC-02, SEC-03 (tested), SEC-04, SEC-05, SEC-06 (layers are never opened),
SEC-07, SEC-08 (uniform failures, constant-time decoy hashing), SEC-09,
SEC-11 (schema and trigger), SEC-12, SEC-13 (key at 0600).

❌ SEC-10 SSRF guards — no webhook dispatcher exists to guard.
❌ Signature verification policies, SBOM handling beyond referrer storage,
scan dispatch, OpenTelemetry.

❌ **The §16.3 SLO table is unverified.** No load test was run, so the latency
and throughput targets are aspirations in this build, exactly what the
specification says they must not be.

---

## §17–21 — Configuration, lifecycle, packaging

✅ Full configuration surface with validation that names the key, the value and
the acceptable range; flags > env > file > defaults; embedded migrations with
checksum drift detection and advisory-locked application.

❌ ACME (`server.tls.mode: acme` fails loudly; use `file` or terminate TLS at a
proxy). ❌ Backup and restore. ❌ The export escape hatch — worth calling out,
because §3.4.5 promises it and a self-hosted product that makes leaving hard is
lying about self-hosting.

◐ Packaging: a two-stage `Dockerfile` builds `registry` and `ui` as separate
images, cross-compiled for `linux/amd64` and `linux/arm64`;
`docker-compose.yml` brings up an evaluation stack with PostgreSQL; and
`contrib/release.sh` publishes both images to Docker Hub as manifest lists,
refusing a dirty tree or an untagged commit and verifying the version stamp and
architecture coverage afterwards. The multi-architecture build and the stamp
check have been run locally. The same script cross-compiles the `mantle` CLI for
darwin and linux on amd64 and arm64, checksums the archives, and attaches them
to a GitHub Release; `contrib/install.sh` is the `curl | sh` installer that
consumes them, with SHA-256 verification and no privilege escalation by
default. Both were exercised end to end against a local HTTP server, including
the tampered-archive and missing-version paths.

The container images are published; **no GitHub Release exists yet**, so
`install.sh` currently reports that nothing has been published and points at
building from source. No CI runs any of it. ❌ No `.deb`/`.rpm`, no release
automation beyond that script, and `mantle install` — the full installer with
preflight, ACME and a systemd unit — remains unimplemented.

---

## Test coverage

| Layer | Status |
|---|---|
| Unit | ✅ digests, hash checkpointing, names, manifests, credentials, storage, config, routing, UI server |
| Log scrubbing (SEC-12) | ✅ the dedicated test the specification names |
| Integration (real HTTP + real Postgres) | ✅ 27 tests: push/pull, protocol edge cases, security, ledger, GC, client compatibility |
| Architecture | ✅ the §7.2 dependency rule, plus CLI and UI client-separation, enforced |
| Race detector | ✅ the whole suite passes under `-race` |
| Conformance suite | ❌ |
| Client matrix (Docker, Podman, BuildKit, …) | ◐ **Docker 29.7.2 pushes and pulls**; Podman, BuildKit, Skopeo and Cosign untested |
| Browser matrix for `mantle-ui` | ◐ the Go server is tested; the read and create-repository flows were driven in real Chrome, the rest only syntax-checked |
| Property / fuzz | ❌ |
| Concurrency | ◐ concurrent blob commit, and concurrent repository auto-creation |
| Chaos | ❌ |
| Performance | ❌ |
| Upgrade | ❌ |

**A real `docker push` and `docker pull` now round-trip**, against the published
container image running under Compose with PostgreSQL. It was worth doing
early: the reference client failed immediately, in two ways this suite could
not see, and both are now covered by tests in
`test/integration/client_compat_test.go`.

- **The token endpoint ignored the OAuth2 form.** It read the scope from the
  query string and the credential from a Basic header — the original Docker
  shape, and the one the test harness speaks. containerd, and so Docker 29,
  posts `grant_type=password` with the scope, username and password in a
  form-encoded body. Those were dropped, the caller authenticated as nobody,
  and a **200** was returned carrying a well-formed token granting nothing.
  Docker then retried the push against it until it gave up, with the visible
  symptom a 401 loop on the blob endpoint and no sign the token service was
  involved.
- **Repository auto-creation raced against itself.** Docker uploads a
  manifest's layers concurrently, so a first push reaches `EnsureRepository`
  several times at once. The upsert named `ON CONFLICT (name)`, but the table
  also carries `UNIQUE (organization_id, name)`, which Postgres checks first —
  so the conflict fell outside the arbiter and surfaced as a raw unique
  violation, failing the push with a 500.

Neither is exotic, and both were invisible to a suite whose client performs the
token dance one particular way. That is the argument for the conformance suite
and the rest of the client matrix, not against them: Podman, BuildKit, Skopeo
and Cosign remain untested, and there is no reason to assume they are happier
than Docker was.

---

## Recommended order of work

1. **Wire the OCI conformance suite into CI** and make it green. Everything
   else is guesswork until this exists (R-05 warns it will consume more time
   than planned; starting now is the mitigation).
2. ~~**Push with a real Docker client** and fix what breaks.~~ Done — Docker
   29.7.2 round-trips. Next in this line is **Podman and Skopeo**, on the
   evidence that the reference client found two bugs in ten minutes.
3. **Implement the S3 driver.** It is the largest missing subsystem and the
   thing that blocks multi-node deployment.
4. **Wire the audit writer** into the handlers. The tamper-evident table is
   useless while nothing writes to it.
5. **Build `mantle export`.** The promise in §3.4.5 is only credible with a
   command behind it.
6. **Retention policy engine**, then the rate limiter, then webhook delivery.
