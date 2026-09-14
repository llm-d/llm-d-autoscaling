---
name: pr-review
description: Review an open pull request for this project. Checks Go code quality (AGENTS.md), test coverage, and Kubernetes security. Posts findings as a GitHub PR comment (idempotent: creates on first run, updates with resolved/new diff on re-runs). Use when the user asks to review a PR. Invoke with /pr-review [PR-number] or /pr-review to auto-detect the current branch's PR.
disable-model-invocation: true
allowed-tools: Bash(gh pr view:*), Bash(gh pr diff:*), Bash(gh pr comment:*), Bash(gh pr list:*), Bash(gh api repos/*/issues/*/comments:*), Bash(gh api repos/*/issues/comments/*:*), Bash(git rev-parse:*), Bash(git log:*), Bash(mktemp:*), Agent, TodoWrite
---

# PR Review

**Arguments:** $ARGUMENTS (PR number, or empty to auto-detect)

## Step 1: Identify PR

```bash
# If $ARGUMENTS is a number, use it. Otherwise auto-detect from current branch.
gh pr view ${ARGUMENTS} --json number,title,url,state,isDraft,body,baseRefName,headRefName 2>/dev/null \
  || gh pr view --json number,title,url,state,isDraft,body,baseRefName,headRefName
```

If the PR is closed or a draft, stop and tell the user.

Record: PR number, title, URL, base branch, head SHA.

```bash
git rev-parse HEAD
```

Also check if the PR already has a review comment from Claude Code. First look for the `<!-- claude-pr-review -->` sentinel (new-style); if not found, fall back to the legacy marker `Generated with [Claude Code]` (pre-sentinel comments). Use `last` to get the most recent match:

```bash
# New-style sentinel
gh api repos/{owner}/{repo}/issues/<number>/comments --jq '[.[] | select(.body | startswith("<!-- claude-pr-review -->"))] | last | select(. != null) | {id: .id, body: .body}'
# Legacy fallback (run only if the above produces no output)
gh api repos/{owner}/{repo}/issues/<number>/comments --jq '[.[] | select(.body | contains("Generated with [Claude Code]"))] | last | select(. != null) | {id: .id, body: .body}'
```

If both produce no output, no prior review comment exists — treat this as the first run. If a JSON object is returned by either query, record its **comment ID** and **full body text** and continue in "update" mode.

## Step 2: Fetch PR Context

```bash
gh pr diff <number>
gh pr view <number> --json files --jq '[.files[].path] | join("\n")'
```

Pass both outputs to all review agents.

## Step 3: Parallel Review

Launch **all four agents simultaneously** in a single message with four parallel Agent tool calls:

**Agent: go-reviewer**
Prompt: "Review this pull request for Go code quality and AGENTS.md compliance.
PR #<number>: <title>
Changed files: <list>
Diff:
<diff>"

**Agent: test-analyzer**
Prompt: "Review this pull request for test coverage.
PR #<number>: <title>
Changed files: <list>
Diff:
<diff>"

**Agent: security-auditor**
Prompt: "Review this pull request for Kubernetes security concerns.
PR #<number>: <title>
Changed files: <list>
Diff:
<diff>"

**Agent: go-reuse-checker**
Prompt: "Review this pull request for reimplemented functionality that already exists in the project's imports or Go stdlib.
PR #<number>: <title>
Changed files: <list>
Diff:
<diff>"

Wait for all four to complete before proceeding.

## Step 4: Aggregate and Post

Collect findings from all four agents. Only include issues with confidence >= 80.

### Idempotent comment posting

**If no existing Claude review comment was found** (first run): write the body to a temp file and post with `gh pr comment --body-file`:

```bash
BODY_FILE=$(mktemp /tmp/review_body_XXXXXX.txt)
# Write the fully rendered Markdown (replace everything below with actual content)
cat > "$BODY_FILE" << 'EOF'
<actual comment body>
EOF
gh pr comment <number> --body-file "$BODY_FILE"
rm -f "$BODY_FILE"
```

**If an existing Claude review comment was found** (re-run): diff the old issues against the new findings and update the existing comment using the same temp-file approach:

```bash
BODY_FILE=$(mktemp /tmp/review_body_XXXXXX.txt)
# Write the fully rendered Markdown (replace everything below with actual content)
cat > "$BODY_FILE" << 'EOF'
<actual comment body>
EOF
gh api --method PATCH repos/{owner}/{repo}/issues/comments/<comment_id> --field body=@"$BODY_FILE"
rm -f "$BODY_FILE"
```

**Important**: In both snippets, replace only the `<actual comment body>` placeholder line with the fully rendered Markdown — leave the `EOF` terminator line intact. The heredoc delimiter is quoted (`'EOF'`) so no shell expansion occurs inside it.

To compute the diff from the old comment body:
- **Resolved**: old issue absent from new findings → `- [x] ~~<description>~~ *(resolved)*`
- **Still open**: issue present in both old and new findings → `- [ ] <description>`
- **New**: issue in new findings but absent from old comment → `- [ ] <description> *(new)*`

**Matching key** (applied in order, stop at first match). Before computing word overlap in any rule, strip the trailing `` — `...` `` anchor suffix from the bullet line (e.g. `` — `path/to/file.go#L42` ``), then tokenize on whitespace (case-insensitive):
1. **Exact anchor**: `file.go#Lnn` references are identical → same issue.
2. **Shifted anchor**: same file path **and** >80% word overlap on the stripped descriptions → same issue at its new location (handles rebases).
3. **No anchor** (e.g. Test Coverage items with only a file path): same file path **and** >80% word overlap on the stripped descriptions → same issue.
4. **No anchor and no file match**: >80% word overlap on the stripped descriptions alone → same issue.

Do not treat a reworded description as new if any of the above rules match.

Preserve per-category section headings. Omit a section entirely if it has no items.

### Comment format — new comment

```
<!-- claude-pr-review -->
### Code Review

**Go Code Quality**
- [ ] <brief description> — `path/to/file.go#L42`
  > AGENTS.md: "<relevant rule>"

**Test Coverage**
- [ ] <brief description> — `path/to/file_test.go`

**Security**
- [ ] <brief description> — `path/to/file.go#L10`

**Library Reuse**
- [ ] <brief description> — `path/to/file.go#L10`

🤖 Generated with [Claude Code](https://claude.ai/code)
```

### Comment format — updated comment (re-run diff example)

```
<!-- claude-pr-review -->
### Code Review

**Go Code Quality**
- [x] ~~<previously reported issue>~~ *(resolved)*
- [ ] <still-open issue> — `path/to/file.go#L42`
- [ ] <newly found issue> *(new)* — `path/to/file.go#L55`

**Security**
- [ ] <still-open security issue> — `path/to/file.go#L10`

🤖 Generated with [Claude Code](https://claude.ai/code)
```

### No issues

If no issues meet the threshold **and** there is no existing review comment, post:

```
<!-- claude-pr-review -->
### Code Review

No issues found. Checked Go conventions, test coverage, security, and library reuse.

🤖 Generated with [Claude Code](https://claude.ai/code)
```

If no issues meet the threshold **and** there IS an existing comment, update it using PATCH to mark all previously open items as resolved:

```
<!-- claude-pr-review -->
### Code Review

**<Category>**
- [x] ~~<previously open issue>~~ *(resolved)*

No new issues found.

🤖 Generated with [Claude Code](https://claude.ai/code)
```

When linking to specific lines, use the full commit SHA:
`https://github.com/llm-d/llm-d-autoscaling/blob/<full-sha>/path/to/file.go#L42-L45`

Print the PR URL when done.
