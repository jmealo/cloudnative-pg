# CloudNativePG Testing Conventions

## Test Label Naming

**IMPORTANT:** All test labels must follow the CloudNativePG project conventions:

### ❌ NEVER USE
- Abbreviated notation like `"ds-e2e"`, `"bkp-rst"`, etc.
- Hardcoded label strings in tests
- CamelCase in label values

### ✅ ALWAYS USE
- Predefined constants from `tests/labels.go`
- Kebab-case for label values (e.g., `"dynamic-storage"`)
- PascalCase for constant names (e.g., `LabelDynamicStorage`)

### Example Pattern

```go
// In tests/labels.go
const (
    // LabelDynamicStorage is a label for selecting dynamic storage tests
    LabelDynamicStorage = "dynamic-storage"
)

// In tests/e2e/*.go
var _ = Describe("Dynamic Storage", Label(tests.LabelDynamicStorage), func() {
    // Test implementation
})
```

### Existing Label Conventions

All labels follow this pattern:
- `LabelBackupRestore` → `"backup-restore"`
- `LabelPostgresConfiguration` → `"postgres-configuration"`
- `LabelServiceConnectivity` → `"service-connectivity"`
- `LabelDynamicStorage` → `"dynamic-storage"`

### Why This Matters

1. **Consistency**: All tests use the same labeling system
2. **Refactoring**: Changing a label value only requires updating one constant
3. **Discovery**: Grep for `Label.*=` finds all available test labels
4. **CI/CD**: Label selectors in CI pipelines reference these exact strings

### Running Tests by Label

```bash
# Correct - using the actual label value
make e2e-test-kind E2E_LABELS="dynamic-storage"

# Correct - combining labels
make e2e-test-kind E2E_LABELS="storage && dynamic-storage"

# WRONG - abbreviated notation not supported
make e2e-test-kind E2E_LABELS="ds-e2e"
```

## Adding New Test Labels

When adding a new test label:

1. Add constant to `tests/labels.go`:
   ```go
   // LabelYourFeature is a label for selecting your-feature tests
   LabelYourFeature = "your-feature"
   ```

2. Use the constant in tests:
   ```go
   var _ = Describe("Your Feature", Label(tests.LabelYourFeature), func() {
       // ...
   })
   ```

3. Document the label in this file and in test suite descriptions

4. NEVER use abbreviated notation (e.g., "yf-e2e")
