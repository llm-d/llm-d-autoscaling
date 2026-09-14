---
name: New Release
about: Propose a new release
title: Release vX.Y
labels: kind/release
assignees: ''

---

- [Introduction](#introduction)
- [Prerequisites](#prerequisites)
- [Release Process](#release-process)
- [Announce the Release](#announce-the-release)
- [Final Steps](#final-steps)

## Introduction

This document defines the process for releasing llm-d-workload-variant-autoscaler.

## Prerequisites

1. Permissions to push to the llm-d-workload-variant-autoscaler repository.

1. Choose whether you are releasing a release candidate or an official release, and set the environment variables accordingly:

   - For a **Release Candidate** (e.g. `v0.9.0-rc.1`):
     ```shell
     export VERSION=v0.9.0-rc.1
     export BRANCH_VERSION=0.9
     export REMOTE=upstream
     ```

   - For an **Official Release** (e.g. `v0.9.0`):
     ```shell
     export VERSION=v0.9.0
     export BRANCH_VERSION=0.9
     export REMOTE=upstream
     export FORK_REMOTE=origin
     ```

1. If needed, clone the llm-d-workload-variant-autoscaler [repo].

   ```shell
   git clone -o ${REMOTE} git@github.com:llm-d/llm-d-workload-variant-autoscaler.git
   ```

## Release Process

### Before Creating the Release Branch

1. If you already have the repo cloned, ensure it's up-to-date and your local branch is clean.

1. Create a new branch from the `main` branch and name it `pre-release-${BRANCH_VERSION}` (e.g., `pre-release-0.9`):

1. Pin the following dependencies:
    - [ ] Set `newTag` to `${VERSION}` in `config/base/manager/kustomization.yaml` for the manager image.

1. Commit and push the changes:

   ```shell
   git add config/base/manager/kustomization.yaml
   git commit -m "Prepare for release ${VERSION}" -s -S
   git push ${REMOTE} pre-release-${BRANCH_VERSION}
   ```

### Create or Checkout branch

1. Release Branch Handling:
   - For a Release Candidate:
     Create a new release branch from the `main` branch. The branch should be named `release-${BRANCH_VERSION}`, for example, `release-0.9`:

     ```shell
     git checkout -b release-${BRANCH_VERSION}
     ```

   - For a Major, Minor or Patch Release:
     A release branch should already exist. In this case, check out the existing branch:

     ```shell
     git checkout release-${BRANCH_VERSION} ${REMOTE}/release-${BRANCH_VERSION}
     ```

1. Push your release branch to the llm-d-workload-variant-autoscaler remote.

    ```shell
    git push ${REMOTE} release-${BRANCH_VERSION}
    ```

### Tag commit and trigger image build

1. Tag the head of your release branch with the version:

     ```shell
     git tag -s -a ${VERSION} -m "llm-d-workload-variant-autoscaler ${VERSION} Release"
     ```

1. Push the tag to the llm-d-workload-variant-autoscaler repo:

     ```shell
     git push ${REMOTE} ${VERSION}
     ```

1. Pushing the tag triggers CI action to build and publish the llm-d-workload-variant-autoscaler image (`ghcr.io/llm-d/llm-d-workload-variant-autoscaler`) to the [ghcr registry].
1. Verify the [CI release workflow] completed successfully before proceeding.
1. Test the steps in the tagged quickstart guide after the PR merges.

### Create the release!

1. Create a [new release]:
    1. Choose the tag that you created for the release.
    1. Use the tag as the release title `${VERSION}`
    1. Click "Generate release notes" to auto-populate the list of PRs and contributors.
    1. Summarize the release notes using an LLM of your choice (e.g., Gemini, Copilot, ChatGPT) with the following prompt:

       ```text
       Please summarize these release notes into three clear sections:
       1. Highlights (key features, performance wins, bug fixes)
       2. Upgrade Steps & Deprecations (configuration changes, deprecated flags/metrics)
       3. Known Issues (if any, otherwise omit)
       Provide results in markdown format, with each section clearly labeled and bulleted where appropriate.
       ```

       Review the generated content, edit it if necessary to ensure accuracy, and then copy and prepend this summary at the very top of the release description box on GitHub.
    1. If this is a release candidate, select the "This is a pre-release" checkbox.
1. If you find any bugs in this process, create an [issue].


### After the Release

## Update the autoscaling guides to reference the new release version.

1. Clone the llm-d repo if you haven't already, ensure it's up-to-date and your local branch is clean.

1. Create a new branch in your fork from the `main` branch and name it `update-autoscaling-guides-${BRANCH_VERSION}` (e.g., `update-autoscaling-guides-0.9`):

   ```shell
   git checkout -b update-autoscaling-guides-${BRANCH_VERSION}
   ```

1. Update the version references in the following files:
   - https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/wva-config/platform/k8s/kustomization.yaml
   - https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/wva-config/platform/ocp/kustomization.yaml

1. Commit and push the changes to your fork:

   ```shell
   git add .
   git commit -m "Update autoscaling guides for release ${VERSION}" -s -S
   git push ${FORK_REMOTE} update-autoscaling-guides-${BRANCH_VERSION}
   ```

1. Create a pull request from your fork to the `main` branch of the llm-d repo, and request a review from the llm-d team.

## After the llm-d Release

Unpin all dependencies that were pinned for the release in the llm-d-workload-variant-autoscaler and llm-d repos, and update the release tracking issue with the final release link.

## Announce the Release

Use the following steps to announce the release.

1. Generate the announcement email content by running the following block in your terminal (make sure `${VERSION}` is set in your current shell):

   ```shell
   cat <<EOF
   Subject: [ANNOUNCE] llm-d-workload-variant-autoscaler ${VERSION} is released

   Hi all,

   We are pleased to announce the release of llm-d-workload-variant-autoscaler ${VERSION}!

   ### Container Images
   * WVA controller: ghcr.io/llm-d/llm-d-workload-variant-autoscaler:${VERSION}

      ### Release Notes
   For more details, please see the GitHub release notes: https://github.com/llm-d/llm-d-workload-variant-autoscaler/releases/tag/${VERSION}
   EOF
   ```

1. Copy the generated subject and body, and send an email to `llm-d-contributors@googlegroups.com`.

1. Add a link to the final release in this issue.

1. Close this issue.

[repo]: https://github.com/llm-d/llm-d-workload-variant-autoscaler
[ghcr registry]: https://github.com/orgs/llm-d/packages?repo_name=llm-d-workload-variant-autoscaler
[new release]: https://github.com/llm-d/llm-d-workload-variant-autoscaler/releases/new
[issue]: https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/new/choose
[CI release workflow]: https://github.com/llm-d/llm-d-workload-variant-autoscaler/actions/workflows/ci-release.yaml
