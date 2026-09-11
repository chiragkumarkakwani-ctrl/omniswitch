# Releases

This page documents OmniSwitch's versioning policy, how to verify a release, the release notes template maintainers use, and the rollback procedure.

## Versioning policy

OmniSwitch follows [SemVer](https://semver.org/) with beta-specific caveats while the project is pre-1.0:

- **`0.x` minor bumps may break configuration.** Until `v1.0.0`, a `0.x.0` minor release may require changes to your `GatewayConfig`. Patch releases (`0.x.y`) never do.
- **Betas are prereleases.** Beta builds are tagged `vX.Y.0-beta.N` (e.g. `v0.2.0-beta.1`) and published as GitHub prereleases. They are not picked up by `install.sh` unless you set `OMNISWITCH_VERSION` explicitly, and they do not move the `latest` container tag or the Homebrew formula.
- **`latest` and Homebrew track stable only.** The `ghcr.io/omniswitch-dev/omniswitch:latest` image tag and the `omniswitch-dev/tap` Homebrew formula only ever point at the most recent non-prerelease (stable) tag. To try a beta via Docker, pull the exact tag (e.g. `ghcr.io/omniswitch-dev/omniswitch:0.2.0-beta.1`).
- **Config `apiVersion` is additive-only during beta.** Fields may be added to `omniswitch.dev/v1` `GatewayConfig` during the beta, but existing fields will not be removed or renamed, and defaults will not change in a way that silently alters behavior. Any exception is called out explicitly in `CHANGELOG.md` and release notes.

## Verifying a release

Every release publishes a `checksums.txt` alongside the archives. To verify a downloaded archive on Linux/macOS:

```bash
curl -fsSLO https://github.com/omniswitch-dev/omniswitch/releases/download/<tag>/checksums.txt
curl -fsSLO https://github.com/omniswitch-dev/omniswitch/releases/download/<tag>/omniswitch_<tag>_<os>_<arch>.tar.gz
sha256sum -c --ignore-missing checksums.txt   # macOS: shasum -a 256 -c --ignore-missing checksums.txt
```

A line ending in `OK` for your archive confirms the download matches what was built and published.

For the container image, compare the pulled digest against the digest shown on the release's GitHub Actions run summary:

```bash
docker pull ghcr.io/omniswitch-dev/omniswitch:<tag>
docker inspect --format '{{index .RepoDigests 0}}' ghcr.io/omniswitch-dev/omniswitch:<tag>
```

## Release notes template

Maintainers use this structure for every tagged release (see past releases for examples):

```markdown
## Highlights
- 2-4 bullets on what's new or fixed and why it matters.

## Install
- Binary: `curl -fsSL https://raw.githubusercontent.com/omniswitch-dev/omniswitch/main/install.sh | sh`
  (for a beta: `OMNISWITCH_VERSION=<tag> curl -fsSL .../install.sh | sh`)
- Docker: `docker run -p 8080:8080 -e OPENAI_API_KEY=... ghcr.io/omniswitch-dev/omniswitch:<tag>`
- Homebrew (stable releases only): `brew install omniswitch-dev/tap/omniswitch`

## Verification
- `omniswitch version` should print `<tag>`.
- `sha256sum -c checksums.txt` (see "Verifying a release" above).

## Known limitations
See [`docs/KNOWN_LIMITATIONS.md`](KNOWN_LIMITATIONS.md).

## Upgrade notes
- Config or behavior changes required to upgrade from the previous release, if any.
```

## Rollback procedure

If a release turns out to be broken:

1. **Mark the GitHub release as a prerelease or draft it out** so `install.sh` (without an explicit `OMNISWITCH_VERSION`) and the `releases/latest` API stop resolving to it.
2. **Do not move `latest`/Homebrew forward for that tag.** Because `latest` and the Homebrew formula only advance on confirmed-good stable releases, a broken tag that is promptly unmarked never reaches users who didn't pin a version.
3. **Re-point Homebrew, if it already advanced,** by re-running the tap update against the last known-good tag (or manually editing the formula in `omniswitch-dev/homebrew-tap` as a stopgap).
4. **Open a `beta-blocker`-labeled issue** describing the break and link it from the release notes of the broken tag.
5. **Ship a fixed patch/beta release** (e.g. `v0.2.0-beta.2`) rather than deleting the broken tag outright — deleting tags breaks anyone who already pinned to it and hides the history of what happened.
6. **Communicate** in GitHub Discussions → Announcements if the break affected anyone who already upgraded.
