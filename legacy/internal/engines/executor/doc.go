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

/*
Package executor provides task execution strategies.

# Overview

The executor package provides three execution strategies for running
optimization tasks:

  - [PollingExecutor]: Fixed-interval execution
  - [ReactiveExecutor]: Event-driven execution (TODO)
  - [HybridExecutor]: Combined approach (TODO)

# Thread Safety

All executor types are safe for concurrent use from multiple goroutines.
*/
package executor
