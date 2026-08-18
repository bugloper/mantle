# Releasing

Maintainers only. This is the runbook for [`contrib/release.sh`](../contrib/release.sh),
which publishes a release, and [`contrib/install.sh`](../contrib/install.sh),
which consumes one.

A release is two channels, published together from one command:

| Channel | Artifact | Consumed by |
|---|---|---|
| Docker Hub | `bugloper/mantle`, `bugloper/mantle-ui` — manifest lists for `linux/amd64` and `linux/arm64` | `docker pull`, [`docs/docker.md`](docker.md) |
| GitHub Release | `mantle` CLI archives for darwin and linux on amd64 and arm64, plus one checksums file | `contrib/install.sh` |

The registry daemon is **only** distributed as an image, and the CLI is **only**
distributed as a binary. That split is deliberate: a daemon needs a database and
a configuration file, so handing one to a `curl | sh` would be presumptuous;
and a CLI in a container is awkward to point at your own filesystem.

---

## The short version

```bash
git tag -a v0.1.0 -m 'v0.1.0'      # a release is a tag, always
contrib/release.sh --dry-run       # build and verify everything, publish nothing
contrib/release.sh                 # publish, taking the version from the tag
```

The tag is pushed by the script, because `gh` cannot attach a release to a tag
the remote does not have. `make release` is the same thing. `make release-dry-run` is the dry run, and
passes `--allow-dirty` so it works mid-change.

---

## What you need first

| Requirement | Needed for | Checked at |
|---|---|---|
| `docker` with `buildx` | the images | preflight |
| A `docker-container` builder | multi-arch manifest lists | created if absent |
| `docker login` to Docker Hub | pushing images | warned, not enforced |
| `go` | cross-compiling the CLI | preflight |
| `gh`, authenticated | creating the GitHub Release | preflight |

Preflight only checks what the run actually needs, so `--skip-binaries` does not
require `gh`, and `--skip-images` does not require Docker.

The buildx builder is created on demand and **left running** between releases
so its cache survives — you will see `buildx_buildkit_mantle-release0` in
`docker ps`. Remove it with `docker buildx rm mantle-release`; the next release
recreates it.

---

## Flags

| Flag | Effect |
|---|---|
| `--dry-run` | Builds and verifies everything, publishes nothing. CLI archives are left in `dist/<version>/`. |
| `--yes` | Skips the typed confirmation. For automation only. |
| `--allow-dirty` | Permits a release from a dirty tree. Testing only. |
| `--skip-images` | No container images. Publishes the GitHub Release alone. |
| `--skip-binaries` | No CLI binaries or GitHub Release. Publishes images alone. |

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `MANTLE_NAMESPACE` | `bugloper` | Docker Hub namespace |
| `MANTLE_PLATFORMS` | `linux/amd64,linux/arm64` | image architectures |
| `MANTLE_BUILDER` | `mantle-release` | buildx builder name |

---

## What it refuses to do

Publishing is public and effectively irreversible — a tag is pullable the
instant it lands, and deleting it afterwards is not the same as never having
published it. So the script is deliberately obstructive:

- **An untagged commit.** A release published from an untagged commit cannot be
  traced back from the version it reports, which makes "which commit is
  production running?" unanswerable — the one question this project exists to
  answer. A dry run is exempt, since a dry run is what you do *before* tagging;
  it falls back to `0.0.0-dryrun`.
- **A dirty working tree.** A published artifact must correspond to a commit
  someone else can check out.
- **A version that is not semantic.** `v1.2.3` or `v1.2.3-rc1`. A tag like
  `latest` or `release-final-2` published as an immutable version is a mess
  nobody can unwind.
- **Publishing without confirmation.** It prints what will be published and
  requires the version to be typed back. `--yes` bypasses this.
- **A tag that points somewhere other than HEAD.** The build always comes from
  the working tree, so a stale tag would label this tree with a version whose
  tag names a different commit — and "what is in v1.2.3?" would then have two
  answers. Move the tag or pick a new version.

## What it verifies afterwards

Three things that fail silently if nobody looks:

- **The version stamp.** A forgotten `--build-arg` yields a perfectly working
  image that reports `dev`, discovered months later by someone asking what is
  in production. The script builds a host-architecture image, runs it, and
  compares what it reports against the version being released.
- **Architecture coverage.** A single-platform push succeeds quietly and is only
  noticed by the person whose architecture is missing. It inspects the pushed
  manifest list and prints every platform in it.
- **That a CLI archive runs.** A cross-compile can produce an unrunnable binary.
  It extracts the archive for the host platform and runs `mantle version`.

---

## Versions and `latest`

The version is the git tag, verbatim — `v0.1.0` produces `:v0.1.0`.

`:latest` moves only for a **stable** release. Any version with a pre-release
suffix leaves it untouched, so someone pulling `:latest` never gets a release
candidate by accident. The same distinction marks the GitHub Release as a
prerelease, which keeps it out of `/releases/latest` and off the "Latest" badge.

A consequence worth knowing: while every release so far is a pre-release,
`docker pull bugloper/mantle` fails, because that means `:latest` and no
`latest` exists. Pre-release tags must be named in full.

---

## How `install.sh` consumes a release

The asset layout is a contract between the two scripts. Change one and you must
change the other:

```
mantle_<version>_<os>_<arch>.tar.gz     containing ./mantle, LICENSE, README.md
mantle_<version>_checksums.txt          sha256, one line per archive
```

The installer resolves a version by **listing** releases and taking the newest,
rather than calling `/releases/latest`. That endpoint excludes pre-releases and
404s when every release so far is one — exactly the state a young project is in,
and a confusing failure to debug from the client side.

It verifies the SHA-256 before extracting, and installs to a directory already
on `PATH`, creating `~/.local/bin` rather than escalating to `sudo`. Overrides:

```bash
MANTLE_VERSION=v0.1.0 sh install.sh          # pin a version
MANTLE_INSTALL_DIR=/usr/local/bin sh install.sh
MANTLE_BASE_URL=http://localhost:8899 sh install.sh    # a mirror, or a test
```

`MANTLE_BASE_URL` is how the installer is tested without publishing anything:
run `contrib/release.sh <version> --dry-run --skip-images`, serve
`dist/<version>/` over HTTP, and point the installer at it.

---

## Testing a release without publishing

```bash
# Build every artifact, publish nothing. Archives land in dist/<version>/.
contrib/release.sh v0.1.0 --dry-run --allow-dirty

# Serve them and run the installer against them.
(cd dist/v0.1.0 && python3 -m http.server 8899) &
MANTLE_BASE_URL=http://127.0.0.1:8899 \
MANTLE_VERSION=v0.1.0 \
MANTLE_INSTALL_DIR=$(mktemp -d) sh contrib/install.sh
```

`dist/` is gitignored.

---

## Two things that will confuse you

**Docker Hub deletion is asynchronous.** Deleting a repository through the API
returns `202 Accepted` while the repository is still live and still answering
`200`. A probe during that window can even return `403 operation requires admin
permissions`, which looks exactly like a permissions failure and is not one.
Wait and re-check before concluding anything; it settles to `404`.

**`docker pull` on a pre-release looks broken.** See the `latest` section above.
The tag has to be given in full.

---

## Gaps

- **No CI.** Releases are cut by hand from a maintainer's machine. Putting this
  behind a tag-triggered workflow is the obvious next step, and the Dockerfile
  already cross-compiles rather than emulating, so a runner needs no QEMU.
- **Nothing is signed.** No cosign signatures, no SBOM, no provenance
  attestation. The checksums file is the only integrity guarantee, and it is
  served from the same host as the artifacts — which protects against
  corruption, not against a compromised release.
- **No `.deb` or `.rpm`**, and `mantle install` — the installer with preflight,
  ACME and a systemd unit described in the specification — is still unbuilt.
  See [`STATUS.md`](STATUS.md).
