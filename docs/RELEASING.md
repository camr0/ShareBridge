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
go vet ./...
go build ./cmd/agent

cd ../control
go test ./... -count=1
go vet ./...
go build ./cmd/server

cd ..
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

Redeploy the control plane with its established operator script:

```bash
cd control
./redeploy.sh
```

### Relay VM (Phase 4a)

The relay runs on its own VM (spec §4.6) and is deployed independently of the
control plane with the hardened installer:

```bash
# Verify the committed deployment artifacts first (no root/systemd needed).
bash relay/deploy/deploy_test.sh

# Then install/upgrade the two hardened services on the relay VM.
relay/deploy/install.sh --tunnel-host <relay-tunnel-host> \
  --sync-url <https://control-sync-endpoint> --sync-san <control-sync-san> \
  --namespace <sbXXXXXXXX> \
  --sync-ca control-ca.crt --sync-cert gateway-sync.crt --sync-key gateway-sync.key \
  --transport-cert tunnel-server.crt --transport-key tunnel-server.key
```

Before enabling relay selection, confirm:

- `deploy_test.sh` is GREEN and `systemd-analyze verify` accepts both units;
- the §6 DNS audit passes (`install.sh --audit-dns`): the relay wildcard and
  the tunnel host are DNS-only and the zone publishes no HTTPS/SVCB/ECH
  records;
- the relay VM holds no content certificate/key and no DNS/ACME credential;
- restart ordering is intact (gateway before frps; frps process-up is never
  presence, and a dead frps is reported unhealthy within the 30-second
  freshness window).

Full runbook: [`docs/operations/phase4a-relay.md`](operations/phase4a-relay.md).

**Relay rollback:** set `RELAY_SELECTION_ENABLED=false` in the control
environment (relay is never selected; direct candidates still get the
interstitial), then stop `sharebridge-relay-frps` and
`sharebridge-relay-gateway` on the relay VM. The v2 production/all-account
deployment keeps the flag false until the release gate (Task 44) records GO.

Deploy the `sharebridge-agent` container using the operator's established self-hosting platform, then monitor the deployment through completion. Never store deployment credentials or other secrets in this repository.

Verify all of the following:

- the agent container is healthy, connects to signaling, and re-registers shares;
- the private agent dashboard responds;
- a representative OpenCloud share loads and downloads correctly;
- a representative large Immich gallery initially renders 120 items and expands progressively;
- bounded control-plane and agent logs contain no new errors.

## 5. Roll back

For the control plane, check out the prior source tag and rerun the established server deployment procedure. For the agent, temporarily pin the prior immutable full-SHA GHCR tag and redeploy it through the established container platform. Repeat the production smoke tests after either rollback and document why it was required.
