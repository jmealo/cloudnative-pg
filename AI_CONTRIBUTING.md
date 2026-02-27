<!--
Guidelines for AI-assisted contributions to cloudnative-pg/cloudnative-pg.
Vendor-agnostic; keep this file in sync with CONTRIBUTING.md and project norms.
Last updated: 2026-02.
-->

# AI Contribution Guidelines for CloudNativePG

Use this document when generating code, PRs, or prompts with AI tooling. Following these rules minimizes review back-and-forth.

## 1) Commit & PR hygiene
- **Conventional Commits**: `type(scope): description` (types: feat, fix, docs, chore, test, ci, refactor). Scope is the subsystem (controller, api, e2e, etc.). Description lowercase; no empty scope.
- **DCO sign-off** on every commit: `Signed-off-by: Name <email>` matching your Git identity.
- **Linear history**: no merge commits; rebase on main; squash fixups.
- **PR title** uses conventional format; PR description includes problem, root cause, approach, risks, compatibility/backport intent, and links issue (`Closes #NNNN`).

## 2) Go code standards
- **Error wrapping**: always `fmt.Errorf("context: %w", err)`; lowercase messages, no trailing punctuation.
- **Logging**: structured logging only (`log.FromContext(ctx).Info/Error` with key/value). No fmt/printf; avoid double-logging and returning.
- **Imports**: Stdlib → third-party → local (`github.com/cloudnative-pg/cloudnative-pg/...`); respect required aliases.
- **Comments**: Godoc on exports starting with identifier. Keep comments accurate.
- **Naming**: idiomatic Go; avoid stutter; receiver names short/consistent.
- **No magic numbers**: prefer named constants.

## 3) Operator patterns
- Reconciles must be idempotent; never mutate a CR **Spec** in reconciliation—only **Status**.
- `context.Context` is always the first arg; check `apierrors.IsNotFound(err)`.
- Use controller-runtime client correctly; set owner refs; handle status updates with conflict retries.

## 4) API & CRD changes
- Optional fields: pointer types, `+optional`, `omitempty`, validation markers.
- Run **both**: `make generate` and `make manifests` after any `api/v1/` change and commit generated code.
- Add webhook validation and negative tests for new fields.

## 5) Testing & docs
- Add/update unit tests (table-driven, testify) for new code paths.
- Add/update E2E tests for behavioral changes; follow existing patterns.
- If user-facing behavior changes, update `docs/src/` in the same PR.
- Provide exact test commands in the PR description.

## 6) AI PR generator prompt (use when drafting PRs)
You are contributing to cloudnative-pg/cloudnative-pg. Goal: produce a PR that merges with minimal reviewer back-and-forth.

Hard requirements:
1. Conventional PR title with correct scope (fix/feat/docs/ci/test/chore + area in parentheses).
2. Tight scope: one logical change; no drive-by refactors.
3. PR description includes problem, root cause, approach, risks, compatibility; add “Closes #…” when applicable.
4. Add/update tests; list exact commands to run.
5. Update docs for any behavior/user-facing change.
6. Explicitly state backport intent and align labels/instructions.
7. Match repository style/patterns; avoid new abstractions unless necessary.
8. Output gofmt’d/lint-clean code; no unused/dead/duplicated logic.

Before finalizing:
- Self-review like a maintainer (naming, scope, coverage, docs).
- Include a reviewer checklist mapping code changes to tests/docs.
- When ambiguous, pick the most conservative, backward-compatible approach and explain it.

## 7) Common blockers/nits to avoid
- Forgetting `make generate` / `make manifests` after API changes.
- Missing DCO sign-off or non-conventional titles.
- Missing tests/docs for behavior changes.
- Bare error returns; non-structured logging.
- Missing `omitempty`/validation on optional API fields.
- Stale comments or scope creep/unrelated reformatting.

## 8) Pre-flight checklist
```
□ Conventional PR title; commits signed off
□ Rebased on main; no merge commits
□ Issue linked (Closes #NNNN)
□ make generate && make manifests (if api/v1 touched)
□ make fmt && make lint && make test
□ Unit tests added/updated; E2E added for behavior changes
□ Docs updated for user-facing changes
□ Errors wrapped with %w; structured logging
□ Godoc on exports; optional fields are pointers with +optional/omitempty/validation
□ Scope is tight; no unrelated changes
```
