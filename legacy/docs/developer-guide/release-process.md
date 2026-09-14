# Release Process

This guide describes how to cut a release of the Workload Variant Autoscaler (WVA). It covers changes in this repository (tag, image), required updates in the llm-d repo's workload-autoscaling guide once the release is out.

## Quick reference

| Step | Where | What |
|------|--------|------|
| 1. Pre-release | This repo | Changelog, optional version bumps (see below) |
| 2. Tag & release | GitHub | Create tag `vX.Y.Z`, push; create GitHub Release (publish) |
| 3. Automation | GitHub Actions | Image build + push |
| 4. Post-release | llm-d repo | **Required:** Update [workload-autoscaling](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling) guide (version callout, CRD/sample URLs) |

---

## Scope of a release

A full release typically involves:

1. **This repo (WVA)**  
   - A version tag (e.g. `v0.5.2`).  
   - A container image built and pushed to `ghcr.io/llm-d/llm-d-workload-variant-autoscaler:<tag>`.  

2. **llm-d repo and guides (required)**  
   - Once the release is out, the [workload-autoscaling](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling) guide in [llm-d/llm-d](https://github.com/llm-d/llm-d) must be updated to the new WVA version so users get consistent instructions and correct CRD/sample URLs.

3. **Documentation in this repo**  
   - Changelog and, if needed, [upstream version pins](../upstream-versions.md).

---

## Prerequisites

- **Permissions:** Ability to push tags and create/publish GitHub Releases.
- **Secrets:** `CR_TOKEN` and `CR_USER` (or equivalent) configured for GHCR so the release workflow can push the image.
- **State of main:** Release from a clean, tested state (e.g. main or a release branch). Run tests locally or rely on CI before tagging.

---

## Pre-release checklist (this repo)

Do these on the branch you intend to tag (e.g. `main`).

1. **Changelog**  
   - Add a release-specific changelog under `docs/` (e.g. `docs/CHANGELOG-v0.5.2.md`) summarizing user-facing and notable changes.  
   - Optionally reference it in release notes when you create the GitHub Release.

2. **Kustomize default image tag (optional)**  
   - If you want the default kustomize install to use the new version, update `config/base/manager/kustomization.yaml`: set `newTag` to the new version (e.g. `v0.5.2`) and commit.  

3. **Upstream dependency pins**  
   - If this release pins a new version of an upstream dependency (e.g. [llm-d-inference-sim](https://github.com/llm-d/llm-d-inference-sim)), update [docs/upstream-versions.md](../upstream-versions.md) and the referenced files (e.g. `test/e2e/fixtures/model_service_builder.go`, `test/utils/resources/llmdsim.go`) before releasing.

---

## Creating the release

### 1. Create and push the tag

Use semantic versioning (e.g. `v0.5.2`). Alternatively, you can **create the tag in step 2** when creating the GitHub Release (no need to run the commands below).

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
```

- Pushing a tag matching `v*` triggers [`.github/workflows/ci-release.yaml`](../../.github/workflows/ci-release.yaml), which builds and pushes the Docker image to `ghcr.io/llm-d/llm-d-workload-variant-autoscaler:<tag>`.

### 2. Create the GitHub Release

- In the repo: **Releases → Create a new release**.
- Choose the tag you just pushed (e.g. `vX.Y.Z`), or **create the tag there** by typing a new tag name (e.g. `vX.Y.Z`); GitHub will create the tag when you publish.
- Add release notes (you can paste from your changelog or `docs/CHANGELOG-vX.Y.Z.md`).
- **Publish** the release.

---

## llm-d repo and guides (post-release, required)

The [llm-d](https://github.com/llm-d/llm-d) repo hosts a **workload-autoscaling guide** that references WVA. Once a release is out, this guide **must** be updated so install instructions and URLs match the new version. Update the following:

1. **Version compatibility note**  
   - In `guides/workload-autoscaling/README.md`, update the "Version Compatibility" callout to state the new WVA version (e.g. "tested and validated with **WVA vX.Y.Z**").

2. **URLs that embed the version tag**  
   - **CRD install:** Any URLs that embed the release tag (e.g. `.../workload-variant-autoscaler/vX.Y.Z/...`) — replace the version segment with the new release tag.
   - **Prometheus Adapter values:** All `curl`/download URLs that point at `.../workload-variant-autoscaler/vX.Y.Z/config/samples/...` (e.g. `prometheus-adapter-values.yaml`, `prometheus-adapter-values-ocp.yaml`).
   - **Upgrading section:** Any CRD or sample URLs in the "Upgrading" section that include the version tag.

3. **Breaking changes and upgrading text**  
   - If the new release has breaking changes, add or update the "Breaking Changes" / "Upgrading" content and migration steps in the README.

**Guide location:** [guides/workload-autoscaling](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling) (main branch). After editing, open a PR in the [llm-d/llm-d](https://github.com/llm-d/llm-d) repo so the guide stays in sync with the WVA release.

---

## Enabling others to cut releases

To allow other team members to perform releases:

1. **Permissions**  
   - Grant them permission to push tags and to create/publish GitHub Releases.

2. **Secrets**  
   - Ensure release workflows have access to the required GHCR credentials (`CR_TOKEN`, `CR_USER` or org equivalent). Document where these are set (e.g. repo or org secrets) in internal runbooks.

3. **Documentation**  
   - Point them to this guide and to the workflows:
     - [`.github/workflows/ci-release.yaml`](../../.github/workflows/ci-release.yaml) — image build on tag push.

4. **Optional runbook**  
   - Maintain a short internal checklist (e.g. "pre-release checklist → tag → release → update llm-d guide") so the steps above are easy to follow without re-reading the full doc.

---

## Summary

| Item | Action |
|------|--------|
| **WVA release** | Tag `vX.Y.Z` → push → create and publish GitHub Release. CI builds and pushes the image. |
| **Guide / llm-d repo** | **Required:** After release, update the [workload-autoscaling](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling) guide (version callout, CRD/sample URLs) and open a PR in llm-d/llm-d. |
| **Team** | Use this doc plus repo permissions and GHCR secrets so others can run the same process safely.

For workflow details, see [`.github/workflows/ci-release.yaml`](../../.github/workflows/ci-release.yaml).
