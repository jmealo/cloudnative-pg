# Claude Code Configuration for cloudnative-pg-gemini

## Ralph Loop for E2E Testing

This directory contains configuration for Ralph Loop, an iterative AI development loop.

### Starting the Ralph Loop

To run E2E tests with the Ralph Loop:

```bash
# Prerequisites (run these first):
az login
az account set --subscription <your-subscription-id>
export CONTROLLER_IMG_BASE=ghcr.io/jmealo/cloudnative-pg-testing

# Start the Ralph Loop:
/ralph-loop --prompt-file RALPH_E2E_TESTING_PROMPT.md --max-iterations 50 --completion-promise 'E2E_TESTS_COMPLETE'
```

### What the Ralph Loop Does

1. Reads the prompt from `RALPH_E2E_TESTING_PROMPT.md`
2. Sets up a stop hook that intercepts when Claude tries to exit
3. Feeds the same prompt back on each iteration
4. Claude sees previous work in files and git history
5. Continues until the completion promise is genuinely true

### Completion Promise

The loop completes when Claude outputs:
```
<promise>E2E_TESTS_COMPLETE</promise>
```

This should only be output when **ALL** 19 E2E tests pass.

### State File

The Ralph Loop state is stored in `.claude/ralph-loop.local.md`:

```yaml
---
active: true           # Whether the loop is active
iteration: 1           # Current iteration number
max_iterations: 50     # Max iterations before auto-stop
completion_promise: "E2E_TESTS_COMPLETE"
started_at: "2026-02-10T01:00:00Z"
repo: "cloudnative-pg-gemini"  # Custom field (preserved)
run_history: []                 # Custom field (preserved)
---

# The prompt content follows...
```

### Cancelling the Ralph Loop

```bash
/cancel-ralph
```

Or manually: `rm .claude/ralph-loop.local.md`

### Monitoring Progress

```bash
# View current iteration:
grep '^iteration:' .claude/ralph-loop.local.md

# View full state:
head -20 .claude/ralph-loop.local.md

# View E2E test logs:
tail -100 /tmp/e2e-run-*.log
```

### Custom Fields

The setup script preserves custom fields like `repo` and `run_history` when restarting loops. This allows tracking across multiple loop invocations.

### Files in This Directory

- `ralph-loop.local.md` - Current Ralph Loop state (gitignored)
- `README.md` - This documentation
