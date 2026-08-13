# Releasing ShareBridge

ShareBridge uses Semantic Versioning tags in the form `vX.Y.Z`. A release covers the product as a whole; transport, envelope, configuration-schema, and upstream API compatibility versions remain independent.

## 1. Prepare the version

1. Choose the next `vX.Y.Z` version.
2. Move curated user-visible entries from `Unreleased` into a dated changelog section.
3. Confirm compatibility notes and deployment requirements.
4. Ensure the tagged tree contains the updated README, changelog, and this checklist.

## 2. Verify the release candidate

Run the complete gate from the repository root. Only run `gofmt` when changed Go files exist, and pass only those files to it; never invoke it with an empty file list.

```bash
cd agent
gofmt -w $(git diff --name-only -- '*.go')
go test ./... -count=1
go test -race ./internal/transfer ./internal/peer ./internal/relaychannel ./internal/multilane -count=1
go vet ./...
go build ./cmd/agent

cd ../signaling-server
go test ./... -count=1
go vet ./...
go build ./cmd/server

cd web
npm test

cd ../..
git diff --check
git status --short
```

Review the final status and do not include unrelated working-tree files in the release commits.

## 3. Publish the tag and GitHub Release

Create an annotated tag from the verified `main` commit:

```bash
git push origin main
git tag -a vX.Y.Z -m "ShareBridge vX.Y.Z"
git push origin vX.Y.Z
gh run list --workflow agent-container.yml --limit 5
gh release create vX.Y.Z --title "ShareBridge vX.Y.Z" --notes-file /path/to/release-notes.md
```

Confirm that the Agent Container workflow succeeds and that GHCR contains both `ghcr.io/camr0/sharebridge-agent:vX.Y.Z` and the immutable full-commit-SHA tag.

## 4. Deploy and smoke-test production

Redeploy the signaling server with its established operator script:

```bash
cd signaling-server
./redeploy.sh
```

Deploy the `sharebridge-agent` stack through Komodo's `DeployStack` API, then monitor the asynchronous deployment through completion. Never store Komodo credentials or other secrets in this repository.

Verify all of the following:

- the agent container is healthy, connects to signaling, and re-registers shares;
- the private agent dashboard responds;
- a representative OpenCloud share loads and downloads correctly;
- a representative large Immich gallery initially renders 120 items and expands progressively;
- bounded signaling-server and agent logs contain no new errors.

## 5. Roll back

For the signaling server, check out the prior source tag and rerun the established server deployment procedure. For the agent, temporarily pin the prior immutable full-SHA GHCR tag in Versa and redeploy through Komodo. Repeat the production smoke tests after either rollback and document why it was required.
