#!/usr/bin/env bash
set -euo pipefail

GOTOOLCHAIN=auto go version
GOTOOLCHAIN=auto go env GOTOOLCHAIN
GOTOOLCHAIN=auto go mod download
