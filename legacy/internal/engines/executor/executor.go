/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package executor

import "context"

// Executor defines how optimization tasks are executed.
type Executor interface {
	// Start begins execution and blocks until context is cancelled.
	Start(ctx context.Context)
}

// OptimizeFunc is the function to be executed.
//
// Deprecated: This name is misleading as it's used for generic task execution,
// not just optimization (e.g., metric fetching, background workers).
// It will be renamed in the future to better reflect its generic purpose.
type OptimizeFunc func(ctx context.Context) error

// Config holds common executor configuration.
type Config struct {
	OptimizeFunc OptimizeFunc
}
