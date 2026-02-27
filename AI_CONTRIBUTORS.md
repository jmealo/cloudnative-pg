# AI_CONTRIBUTORS.md

> **System Instructions for Autonomous Coding Agents**
> Generated from analysis of 200+ merged PRs in cloudnative-pg/cloudnative-pg
> Last updated: 2026-02-09
> Use this with [AI_CONTRIBUTING.md](./AI_CONTRIBUTING.md) for complete guidance

This document captures the **social and technical patterns** observed in actual code reviews. It teaches autonomous agents to pass peer review without friction by understanding what reviewers actually care about.

---

## 1. Repository Personality

**Culture: Professional, Rigorous, Fast-Moving**

CloudNativePG follows a **high-trust, fast-approval model** for contributors who demonstrate understanding of the codebase patterns:

- **Velocity**: Small, well-scoped PRs (size:XS/S) merge in 0.2-9 hours when they follow conventions
- **Larger changes** (size:L) take 48-107 hours due to comprehensive review
- **Review philosophy**: Two maintainers must approve; reviewers trust contributors who follow established patterns
- **Testing emphasis**: CI must pass; E2E tests are run extensively; flaky tests are aggressively fixed
- **Documentation culture**: User-facing changes MUST update docs in same PR
- **Backport-first mindset**: Most PRs target multiple release branches (1.25, 1.27, 1.28, main)

**Development Model:**
- Trunk-based development on `main`
- Linear history (squash/rebase, no merge commits)
- Branch naming: `dev/ISSUE_ID` (maintainers) or feature branches (external contributors)
- Conventional commits strictly enforced
- DCO sign-off required on every commit

**Review Expectations:**
- Reviewers look for: proper error handling, structured logging, test coverage, documentation
- Reviewers trust: idiomatic Go, proper API patterns, backward compatibility
- Reviewers reject: scope creep, drive-by refactors, missing tests, breaking changes without migration

---

## 2. The SME Map: Who Reviews What

This table maps directories to Subject Matter Experts based on 200+ merged PRs. Tag these reviewers when your PR touches these paths:

| Directory/Scope | Primary Reviewers (Review Count) | Notes |
|-----------------|----------------------------------|-------|
| `.github/` | @sxd (188), @mnencia (68), @armru (3) | CI/CD workflows, GitHub Actions |
| `docs/` | @gbartolini (9), @sxd (9), @leonardoce (7) | Documentation, release notes |
| `hack/` | @mnencia (8), @sxd (8), @NiccoloFei (1) | Build scripts, release automation |
| `internal/` | @mnencia (51), @sxd (14), @NiccoloFei (1) | Core operator logic, controllers |
| `pkg/` | @armru (6), @gbartolini (6), @NiccoloFei (4) | Shared packages, PostgreSQL mgmt |
| `tests/` | @NiccoloFei (13), @armru (10), @gbartolini (4) | E2E tests, test infrastructure |
| `releases/` | @leonardoce (2), @mnencia (2), @sxd (2) | Release manifests, versioning |
| Root files | @mnencia (27), @sxd (23), @NiccoloFei (2) | CONTRIBUTING, Makefile, etc. |

**How to use this:**
- Multi-file PRs will naturally attract the right reviewers
- Mention specific SMEs in PR description if you need their input: "cc @mnencia for controller changes"
- Don't tag for trivial changes (typos, dependency updates)

---

## 3. The "Nitpick" Blocklist: Do Not Do This

Extracted from actual PR review comments that caused change requests or extended discussion:

### Code Quality

❌ **Don't: Return bare errors**
```go
return err
```
✅ **Do: Wrap errors with context**
```go
return fmt.Errorf("failed to reconcile cluster: %w", err)
```
- Error messages lowercase, no trailing punctuation
- Always use `%w` for error wrapping

❌ **Don't: Use fmt.Printf or log.Println**
```go
fmt.Printf("Processing cluster %s
", name)
```
✅ **Do: Use structured logging**
```go
log.FromContext(ctx).Info("Processing cluster", "clusterName", name)
```

❌ **Don't: Rely on external cleanup in tests**
```go
// No cleanup needed, namespace is deleted elsewhere
```
✅ **Do: Make tests self-contained**
```go
defer func() {
    // Cleanup resources even if test fails
    testEnv.DeleteCluster(clusterName)
}()
```
- Tests should not rely on "current state of code" for cleanup
- Each test should be independently runnable

### Documentation & Organization

❌ **Don't: Put kubectl plugin changes in main release notes**
- Reviewers will ask: "Should this be in the plugin section?"
- Check existing release note structure before adding

❌ **Don't: Forget `make generate` and `make manifests` after API changes**
- This is a blocker; CI will fail
- Always run both commands and commit generated code

❌ **Don't: Add unrelated reformatting or refactoring**
- Tight scope is valued; drive-by cleanups delay merge
- If you see something worth fixing, open a separate issue

### API & Configuration

❌ **Don't: Skip validation on configuration**
```go
if config.Enabled {
    // Use watchNamespaces
}
```
✅ **Do: Validate incompatible configurations**
```go
if config.Enabled && watchNamespaces != operatorNamespace {
    return fmt.Errorf("watchNamespaces must equal operatorNamespace when this feature is enabled")
}
```

❌ **Don't: Add optional fields without proper markers**
```go
type Spec struct {
    OptionalField string
}
```
✅ **Do: Use pointers with proper tags**
```go
type Spec struct {
    // +optional
    OptionalField *string `json:"optionalField,omitempty"`
}
```

### Commit & PR Hygiene

❌ **Don't: Use non-conventional commit messages**
- `Update code` ← rejected
- `Fixed bug` ← rejected

✅ **Do: Use conventional commits**
- `fix(controller): prevent nil pointer on cluster delete`
- `docs(backup): clarify retention policy examples`
- `test(e2e): add coverage for timeline validation`

❌ **Don't: Include merge commits**
- Linear history is required; use rebase

---

## 4. The Golden Standard: PRs That Merged With Zero Comments

Analysis of PRs that received immediate LGTM with no review discussion:

### PR Title Format (Strictly Conventional)

**Pattern:** `type(scope): imperative description`

**Real examples:**
- `fix(release): preserve RPM release number in kubectl-plugin.md`
- `ci(openshift): add timeouts to E2E job to prevent orphaned clusters`
- `docs(release-notes): move #9386 to fixes and correct examples`
- `chore: update bug issue template versions`

**Types:** fix, feat, docs, chore, test, ci, refactor, perf
**Scopes:** release, openshift, e2e, controller, api, walrestore, postgres, backup, instance, deps, etc.

### PR Description Template

**Structure observed in clean PRs:**

```markdown
## Problem
[One-sentence description of what's broken or missing]

## Root Cause
[Why does this problem occur? What's the underlying issue?]

## Solution
[What does this PR do to fix it? Keep it concise.]

## Testing
- Unit tests: `make test`
- E2E tests: `make e2e-test-kind FEATURE_TYPE=smoke,basic`
- Verified: [specific scenario tested]

## Compatibility
- Backward compatible: Yes/No
- Backport targets: release-1.28, release-1.27, release-1.25

## References
Closes #ISSUE_NUMBER
```

**Real example (PR #9870):**
> Add automatic retry mechanism to handle transient Kubernetes API proxy errors and network failures commonly occurring in managed environments like AKS.
>
> The implementation uses 3 attempts with exponential backoff (2s, 4s, 8s) and distinguishes between retryable infrastructure errors (proxy failures, network timeouts, HTTP 500s) and permanent errors (command failures, NotFound) which fail immediately without retry.
>
> This addresses E2E test flakiness on AKS where transient proxy errors caused test failures despite the underlying system being healthy.

**Key traits:**
- States the problem clearly
- Explains the "why" (root cause)
- Describes the approach
- Specifies what was tested
- No fluff, no marketing language

### Commit Message Format

**Format:** Same as PR title for single-commit PRs

**For multi-commit PRs:**
- Each commit follows conventional format
- Commits tell a story: test → implementation → docs → fix
- Squash fixup commits before merge

**Sign-off required:**
```
fix(controller): handle nil pointer in reconciliation

Check cluster reference before accessing fields to prevent panic.

Signed-off-by: Your Name <your.email@example.com>
```

### Labels Applied to Golden PRs

Auto-applied by bots and maintainers:
- **Status:** `lgtm`, `ok to merge :ok_hand:`
- **Size:** `size:XS`, `size:S`, `size:M`, `size:L`
- **Type:** `bug :bug:`, `documentation :book:`, `chore`, `github_actions`
- **Backport:** `backport-requested :arrow_backward:`, `release-1.25`, `release-1.27`, `release-1.28`
- **Issues:** `no-issue` (if no linked issue)

Don't add labels yourself; maintainers will apply them.

### File Organization in PRs

**Observed patterns:**
1. **API changes:** `api/v1/*.go` + `config/crd/*.yaml` + generated code
2. **Controller changes:** `internal/management/controller/*.go` + `internal/management/controller/*_test.go`
3. **Tests:** `tests/e2e/*_test.go` + `tests/utils/*.go` (helper functions)
4. **Docs:** `docs/src/*.md` (updated in same PR as code changes)

**Clean PRs touch 1-4 files:**
- 1-2 files: XS (merges in hours)
- 3-5 files: S (merges in 1 day)
- 6-15 files: M (merges in 2-3 days)
- 16+ files: L (merges in 4-7 days, requires extensive review)

---

## 5. Automation & Workflow Triggers

### Bot Comments You'll See

**`@github-actions` bot:**
- Backport notifications: "By default, the pull request is configured to backport to all release branches."
- Test trigger responses: `@username, here's the link to the E2E on CNPG workflow run: [URL]`

**`@cnpg-bot`:**
- License refresh PRs
- Automated maintenance tasks

### Commands You Can Use

**`/test`** - Triggers E2E test suite
- Usage: Comment `/test` on your PR
- Can specify feature type: `/test ft=smoke`

**`/ok-to-merge`** - Signals ready for merge (maintainers only)

### CI Checks That Must Pass

From `.github/workflows/`:
1. **golangci-lint** - Code linting
2. **unit tests** - `make test`
3. **e2e tests** - Full test suite on kind cluster
4. **DCO check** - All commits signed off
5. **conventional commit check** - PR title validation
6. **license check** - Dependency licenses validated
7. **code generation check** - Ensures `make generate` was run

---

## 6. Pre-Submission Checklist for AI Agents

Before creating a PR, verify:

```
□ PR title: type(scope): lowercase description
□ All commits: Signed-off-by line present (git commit -s)
□ Branch: Rebased on latest main, linear history
□ Issue: Linked with "Closes #NNNN" in description
□ API changes: Ran make generate && make manifests
□ Code: Ran make fmt && make lint && make test (all pass)
□ Tests: Added unit tests for new code paths
□ Tests: Added/updated E2E tests for behavior changes
□ Docs: Updated docs/src/*.md for user-facing changes
□ Errors: All errors wrapped with fmt.Errorf("%w", err)
□ Logging: No fmt.Printf; only structured logging
□ Scope: Tight focus; no unrelated changes
□ Description: Includes problem, solution, testing, compatibility
```

**If ANY checkbox is unchecked, do NOT submit the PR yet.**

---

## 7. Common Scenarios & Expected Outcomes

### Scenario: Fixing a bug in controller reconciliation

**Expected PR structure:**
- Title: `fix(controller): prevent panic when cluster is nil`
- Files: `internal/management/controller/cluster_controller.go`, `*_test.go`
- Tests: Unit test reproducing the panic, E2E test if behavior change
- Description: Explains when nil happens, what breaks, how fix prevents it
- Merge time: 24-48 hours (requires thorough review of controller logic)

### Scenario: Adding a new API field

**Expected PR structure:**
- Title: `feat(api): add backupRetentionDays to backup spec`
- Files: `api/v1/backup_types.go`, `config/crd/*.yaml`, generated files, docs, webhook validation
- Must run: `make generate && make manifests`
- Tests: Unit tests for validation, E2E test using new field
- Description: Explains use case, default value, backward compatibility
- Merge time: 48-72 hours (API changes get extra scrutiny)

### Scenario: Updating documentation

**Expected PR structure:**
- Title: `docs(backup): clarify retention policy examples`
- Files: `docs/src/backup_recovery.md`
- No code changes, no tests needed
- Description: Brief explanation of what was unclear
- Merge time: 2-8 hours (docs PRs merge fast if clear)

### Scenario: Improving E2E test reliability

**Expected PR structure:**
- Title: `test(e2e): add retry logic for transient API errors`
- Files: `pkg/utils/exec.go`, `pkg/utils/exec_test.go`, potentially test helpers
- Tests: Unit test for retry logic, verify E2E tests pass consistently
- Description: Explains failure mode, retry strategy, why it's safe
- Merge time: 24-48 hours

---

## 8. What Reviewers Actually Look For

Based on inline review comments and approval patterns:

### First Pass (Initial Review)
1. **PR title and description** - Is this conventional? Is problem clear?
2. **Scope** - Is this one logical change or multiple unrelated changes?
3. **Tests** - Are there tests? Do they cover the change?
4. **Docs** - If user-facing, are docs updated?
5. **CI status** - Are checks green?

### Deep Review (Second Pass)
1. **Code correctness** - Logic bugs, edge cases, error handling
2. **Operator patterns** - Idempotent reconciliation, proper status updates
3. **API compatibility** - Breaking changes? Migration path?
4. **Test quality** - Do tests actually validate the fix? Are they flaky?
5. **Performance** - Any obvious bottlenecks?

### What Gets a Quick LGTM
- Clear, focused changes
- Obvious correctness
- Well-tested
- Follows existing patterns
- Good commit messages

### What Triggers Extended Discussion
- Unclear scope or motivation
- Missing tests or docs
- Deviation from established patterns
- Potential breaking changes
- Performance concerns

---

## 9. Language & Tone Norms

**Observed communication style:**

✅ **Professional, direct, helpful:**
- "I think there's an extra w in the url"
- "This goes into the cnpg plugin section, not here, right?"
- "Let me know if you like my suggestion. Otherwise, I will merge your version."

✅ **Constructive pushback:**
- "I don't think it's good practice to rely on the current state of code to take a decision if to add cleanup or not, because we are pushing the burden of adding a deletion check on a previous test when someone will write a new test."

❌ **Avoid:**
- Vague comments: "This could be better"
- Demands: "Change this immediately"
- Unnecessary praise: "Awesome work!!!" (factual approval is enough)

**When receiving feedback:**
- Acknowledge: "Fix and squashed" or "check now"
- Ask for clarification if needed
- Don't argue unless you have strong technical justification

---

## 10. Red Flags That Will Delay Your PR

### Instant Rejection Triggers
1. **No DCO sign-off** - Auto-fails CI
2. **Non-conventional title** - Auto-fails CI
3. **Merge commits in history** - Violates linear history requirement
4. **Failed tests** - Cannot merge with red CI

### Extended Review Triggers
1. **Missing tests** - "Where are the tests for this?"
2. **Missing docs** - "This changes user-facing behavior; please update docs"
3. **Scope creep** - "This PR does too many things; please split"
4. **API changes without `make generate`** - "Generated code is out of sync"
5. **Bare error returns** - "Please wrap errors with context"
6. **Non-structured logging** - "Use log.FromContext(ctx).Info() instead"

### Signs Your PR Needs Work
- CI fails on any check
- Multiple reviewers request changes
- No responses after 48 hours (likely waiting on you)
- Comments asking "Why?" or "What's the use case?"

---

## 11. How to Recover From Mistakes

### If you forgot DCO sign-off:
```bash
git commit --amend --signoff
git push --force-with-lease
```

### If you need to rebase:
```bash
git fetch upstream
git rebase upstream/main
git push --force-with-lease
```

### If CI says "generated code out of sync":
```bash
make generate
make manifests
git add .
git commit --signoff -m "chore: update generated code"
```

### If reviewer requests changes:
1. Make the changes in new commits (easier to review)
2. Respond to each comment: "Done" or "Fixed in [commit hash]"
3. Wait for re-review
4. Squash fixup commits before merge (maintainers may do this for you)

---

## 12. Success Metrics: Know When You've Nailed It

**A perfect PR receives:**
- ✅ LGTM from two different maintainers
- ✅ `ok to merge` label
- ✅ Merge within 24-48 hours (for small PRs)
- ✅ Zero change requests
- ✅ All CI checks green
- ✅ Automatic backport to release branches (if labeled)

**You're on the right track if:**
- Reviewers approve without inline comments
- Only automated bot comments appear
- Maintainers apply labels quickly
- Your PR appears in next release notes

**You need improvement if:**
- Multiple rounds of review feedback
- Scope reduction requested
- CI failures requiring fixes
- Extended discussion about approach

---

## Final Note for AI Agents

This document captures **actual human behavior patterns**, not just stated rules. When in doubt:

1. **Mimic successful PRs** - Look at recently merged PRs by active contributors
2. **Check AI_CONTRIBUTING.md** - For technical guidelines
3. **Ask before assuming** - Tag maintainers if approach is unclear
4. **Ship small, ship often** - Smaller PRs merge faster and with less friction

The goal is not perfection; it's **predictability and trust**. Show you understand the patterns, and reviewers will trust your changes.

---

*This document is complementary to [AI_CONTRIBUTING.md](./AI_CONTRIBUTING.md). Use both together for optimal results.*
