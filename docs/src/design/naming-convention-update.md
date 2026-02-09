# Test Naming Convention Enforcement - February 9, 2026

## Summary

Updated project documentation and code to prevent the use of abbreviated test label notation (e.g., "ds-e2e") and enforce CloudNativePG project conventions.

## Changes Made

### 1. Documentation Created

**`docs/src/design/testing-conventions.md`** - New comprehensive guide documenting:
- ❌ What NOT to do: abbreviated notation like "ds-e2e"
- ✅ What TO do: use predefined constants from `tests/labels.go`
- Pattern explanation: `LabelDynamicStorage = "dynamic-storage"`
- Examples of correct usage in test files
- CI/CD integration guidance

### 2. In-Code Documentation

**`tests/labels.go`** - Added header comments:
```go
// NAMING CONVENTION:
// - Constant names: PascalCase with "Label" prefix (e.g., LabelDynamicStorage)
// - Constant values: kebab-case (e.g., "dynamic-storage")
// - NEVER use abbreviated notation (e.g., "ds-e2e") - always use full descriptive names
// - See docs/src/design/testing-conventions.md for full guidelines
```

### 3. Test Implementation Fixed

**`tests/e2e/dynamic_storage_test.go`** - Updated to use constant:
```go
// Before (hardcoded string):
var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, "dynamic-storage"), func() {

// After (using constant):
var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, tests.LabelDynamicStorage), func() {
```

### 4. Implementation Guides Updated

**`RALPH_PROMPT.md`** - Added explicit naming convention guidance:
```markdown
2. **Naming Convention**: Use `tests.LabelDynamicStorage` (resolves to `"dynamic-storage"`)
   following CNPG project conventions. NEVER use abbreviated notation like "ds-e2e".
```

**`ralph-dynamic-storage.md`** - Added critical naming section and code comments:
```go
// IMPORTANT: Use tests.LabelDynamicStorage constant, NOT hardcoded "ds-e2e" or other abbreviations
// Follow CNPG project conventions: tests/labels.go defines LabelDynamicStorage = "dynamic-storage"
var _ = Describe("Dynamic Storage", Label(tests.LabelStorage, tests.LabelDynamicStorage), func() {
```

## Verification

### No Existing Violations Found
```bash
# Searched entire codebase for "ds-e2e":
find . -type f \( -name "*.md" -o -name "*.txt" -o -name "*.yml" -o -name "*.go" \) \
  -exec grep -l "ds-e2e" {} \;
# Result: No files found
```

### Current State Compliant
All existing tests follow the convention:
- `tests/labels.go` defines `LabelDynamicStorage = "dynamic-storage"`
- Tests use `Label(tests.LabelDynamicStorage)` (constant reference)
- No hardcoded label strings in test files (now fixed)

## Pattern Reference

### CloudNativePG Label Convention

| Constant Name | Label Value | Usage |
|--------------|-------------|--------|
| `LabelBackupRestore` | `"backup-restore"` | Backup and restore tests |
| `LabelPostgresConfiguration` | `"postgres-configuration"` | PostgreSQL config tests |
| `LabelServiceConnectivity` | `"service-connectivity"` | Service connection tests |
| `LabelDynamicStorage` | `"dynamic-storage"` | Dynamic storage tests |

### Anti-Pattern Examples to AVOID

| ❌ Wrong | ✅ Correct | Reason |
|---------|-----------|---------|
| `"ds-e2e"` | `tests.LabelDynamicStorage` | Use full descriptive names |
| `"bkp-rst"` | `tests.LabelBackupRestore` | Use full descriptive names |
| `"dynamic-storage"` (hardcoded) | `tests.LabelDynamicStorage` | Use constant for refactorability |

## Future Prevention

### For Developers
1. Always import and use `tests` package constants
2. Never hardcode label strings in test files
3. Refer to `docs/src/design/testing-conventions.md` when adding new labels

### For Code Reviewers
1. Check that new tests use label constants from `tests/labels.go`
2. Reject PRs using hardcoded label strings
3. Reject PRs using abbreviated notation (e.g., "ds-e2e", "bkp-rst")

### For AI Assistants
Updated prompts (RALPH_PROMPT.md, ralph-dynamic-storage.md) now explicitly state:
- "NEVER use abbreviated notation like 'ds-e2e'"
- "Use tests.LabelDynamicStorage constant"
- Reference to testing-conventions.md for full guidelines

## Files Modified

1. `docs/src/design/testing-conventions.md` - **NEW**: Comprehensive guide
2. `tests/labels.go` - Added convention documentation in header comments
3. `tests/e2e/dynamic_storage_test.go` - Fixed to use constant instead of hardcoded string
4. `RALPH_PROMPT.md` - Added naming convention requirement
5. `ralph-dynamic-storage.md` - Added naming convention warnings in code examples

## Impact

- **Zero breaking changes**: All existing code already follows the convention
- **Documentation gap closed**: Convention is now explicitly documented
- **Future compliance**: AI assistants and developers have clear guidance
- **Maintainability**: Easier to refactor labels when using constants
