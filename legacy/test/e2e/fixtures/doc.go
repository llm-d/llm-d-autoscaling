// Package fixtures provides shared Kubernetes helpers for the consolidated e2e suite.
//
// Helpers follow a small set of conventions:
//   - Create* fails if the object already exists.
//   - Ensure* is idempotent for test setup: delete-then-recreate (or no-op when ready and spec matches).
//   - Delete* ignores NotFound.
//   - Parameters named baseName are logical names; fixture code may append suffixes (for example
//     "-service", "-monitor", "-decode") to form the actual resource name.
//   - Shared literals such as test-resource and common ports live in model_service_conventions.go.
//
// After changing fixture contracts or generated object shape, compile e2e packages with:
//
//	go test ./test/e2e/... -run TestDoesNotExist
//
// Full e2e runs use make targets (see docs/developer-guide/testing.md and test/e2e/README.md).
package fixtures
