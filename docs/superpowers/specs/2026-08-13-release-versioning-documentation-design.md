# Release Versioning and Documentation Design

## Goal

Establish ShareBridge's first formal release at `v0.8.0` with enough project
documentation and release discipline that the tag identifies a usable,
self-explanatory source tree rather than an undocumented code snapshot.

## Release Identity

ShareBridge follows Semantic Versioning beginning at `v0.8.0`. The `0.8`
starting point reflects a substantial deployed product whose compatibility and
operations may still evolve before `1.0`; it is not a percentage-complete
score.

- Pre-`1.0` minor releases may include intentional compatibility changes, but
  those changes must be called out in the changelog and release notes.
- Patch releases contain backward-compatible fixes and documentation updates.
- `v1.0.0` is reserved for an explicit stable compatibility and upgrade policy.

The product release version is independent from the WebRTC/relay transport
protocol version, binary envelope version, configuration schema version, and
the detected upstream Immich API version. Those versions continue to change
only when their own compatibility boundaries require it.

No runtime version constant or `--version` command is added for `v0.8.0`.
Build-time version reporting and automated release tooling are deferred to the
`0.9` cycle after the manual process has been exercised.

## Canonical Documentation

### Project README

Create a top-level `README.md` as the canonical project entrance. It explains:

- the recipient experience and the problem ShareBridge solves;
- the deployed server, private agent, and browser architecture;
- direct WebRTC and end-to-end encrypted secure-relay modes;
- OpenCloud file shares and Immich album shares;
- current capabilities, including streaming, seeking, Download All, and
  progressive large-gallery loading;
- repository layout, local test commands, and deployment surfaces;
- pre-`1.0` compatibility expectations;
- links to the signaling-server README, changelog, release checklist, roadmap,
  and detailed design documents.

The README describes the real current implementation. It does not present
planned native HTTPS passthrough or other roadmap designs as shipped features.

### Changelog

Create `CHANGELOG.md` using Keep a Changelog structure with:

- an empty `[Unreleased]` section for future work;
- a retrospective `[0.8.0]` entry dated 2026-08-13;
- user-visible groups such as Added, Changed, Fixed, and Security;
- concise descriptions of the product's current capabilities rather than a
  commit-by-commit history;
- prominent coverage of Immich v2/v3 compatibility and progressive loading of
  multi-thousand-item albums.

The changelog links compare pages using the repository's GitHub URL. Because
`v0.8.0` is the first formal SemVer release, its link points directly to the
release tag rather than comparing against the historical `v0-webrtc` tag.

### Release Checklist

Create `docs/RELEASING.md` as the repeatable operator workflow:

1. choose a SemVer version and curate `CHANGELOG.md`;
2. run complete agent, server, browser, race, vet, and build verification;
3. commit the release documentation;
4. create and push an annotated `vX.Y.Z` tag;
5. create a GitHub Release using a concise changelog-derived summary;
6. confirm the versioned and immutable-SHA GHCR agent images;
7. deploy the signaling server with its established redeploy script;
8. deploy the agent through the established container platform and wait for completion;
9. verify the deployed image, signaling connection, and representative live
   file/gallery flows;
10. document rollback using the prior source tag and immutable agent image.

The checklist keeps releases manual for now. It must distinguish publishing a
tag from deploying production and must require evidence at every stage.

## Tag-triggered Agent Image

The existing agent workflow publishes `latest`, full-SHA, and Git-tag image
tags. Adjust its event filtering so any pushed `v*` tag runs the workflow even
when the tagged release commit contains documentation only. Branch and pull
request builds remain path-filtered to agent/workflow changes.

The workflow continues to test and vet before publishing. A `v0.8.0` tag must
produce `ghcr.io/camr0/sharebridge-agent:v0.8.0` and the full commit-SHA tag.
The release process verifies those outputs before deployment.

## First Release Boundary

The release documentation and workflow correction are committed after the
already deployed progressive-gallery implementation. The annotated `v0.8.0`
tag points at this new documentation commit, so the tagged tree contains its
own README, changelog, and release instructions while retaining the exact
production code already validated.

Creating or pushing the tag, creating the GitHub Release, and redeploying are
separate execution steps after the documentation implementation is reviewed
and verified. No tag is created merely by adding these files.

## Validation

- Check all documentation links and commands against the repository layout.
- Confirm the README does not claim unshipped roadmap work.
- Confirm changelog statements are supported by current code or existing
  component documentation.
- Validate workflow YAML and inspect event-filter semantics for branch, pull
  request, and tag pushes.
- Run `git diff --check` and verify only release-scoped files are staged.
- Before tagging, run the complete verification suite specified in
  `docs/RELEASING.md`.

## Deferred Work

- Runtime `--version` output and build metadata injection.
- Automated changelog generation or release pull requests.
- Automated GitHub Release creation.
- A signaling-server container publication pipeline.
- A formally declared `1.0` compatibility and deprecation policy.
