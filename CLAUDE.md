# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`plasmactl-bump` is a launchr plugin to update the version of Ansible roles which were updated in last commit. It detects modified resources (Ansible roles) via git history, bumps their versions, and propagates version changes through dependency chains across the Plasma platform.

**Module**: `github.com/skilld-labs/plasmactl-bump/v2`
**Go version**: 1.25.0 (CGO disabled)
**Framework**: [launchr](https://github.com/launchrctl/launchr) plugin system

## Build & Development Commands

```bash
make build          # Build binary to ./bin/launchr
make test           # Run all tests (requires gotestfmt)
make test-short     # Run short tests only
make lint           # Run golangci-lint with auto-fix
make deps           # Download Go dependencies
make all            # deps + test-short + build
make clean          # Remove ./bin/
DEBUG=1 make build  # Build with debug symbols
```

## Architecture

### Plugin Registration

The plugin registers via `init()` in `plugin.go`, using `//go:embed` for YAML action definitions. Entry point is `cmd/launchr/main.go` which imports the plugin as a blank import.

### Three Actions

1. **bump** (`actionBump.go`): Detects files changed since the last "Bumper"-authored commit, maps them to resources, and sets each resource's version to the commit's short hash (first 13 chars) in `meta/plasma.yaml`. Creates a commit authored by `Bumper <no-reply@skilld.cloud>`.

2. **bump --sync** (`actionSync.go`, `actionSync.resources.go`, `actionSync.variables.go`): Propagates versions through dependency chains. Works on a post-`compose` build directory (`.compose/build`). Builds a chronological timeline of version changes, then applies propagation so dependent resources get composite versions like `original-propagated`.

3. **dependencies** (`dependencies.go`): Shows dependency tree for a given resource.

### Core Packages

- **`pkg/repository`** (`git.go`): Git operations via `go-git`. `Bumper` struct handles commit traversal, detecting own commits, and creating bump commits. All `PlainOpen` calls use `PlainOpenWithOptions` with `EnableDotGitCommonDir: true` to support git worktrees.

- **`pkg/sync`**: Resource/variable management and dependency resolution.
  - `inventory.go`: Builds resource dependency graph by walking filesystem, parsing `tasks/*.yaml` for `include_role` references, topologically sorting with `topsort`.
  - `inventory.resource.go`: `Resource` type, `OrderedMap[T]` generic ordered map, MRN (Machine Resource Name) format `platform__kind__role`, version read/write from `meta/plasma.yaml`.
  - `inventory.variable.go`: Variable tracking from `group_vars/vars.yaml` and `vault.yaml`, variable-to-variable and variable-to-resource dependency maps.
  - `timeline.go`: `TimelineItem` interface with `TimelineResourcesItem` and `TimelineVariablesItem` implementations, chronological sorting.
  - `filesCrawler.go`: File discovery with Jinja2 template variable extraction.
  - `yaml.go`: YAML parsing with ansible-vault support.

### Key Conventions

- **MRN format**: `platform__kind__role` (double underscore separator), e.g. `interaction__softwares__grafana`
- **Resource path pattern**: `{platform}/{kind}/roles/{role}/meta/plasma.yaml`
- **Valid kinds**: applications, services, softwares, executors, flows, skills, functions, libraries, entities
- **Version format**: Short git hash (`abc1234567890`) or composite `baseversion-propagatedversion`
- **Bump commit author**: `Bumper` with email `no-reply@skilld.cloud`, message `versions bump`

### Sync Flow (Propagation)

1. Initialize inventory from `.compose/build` directory
2. Gather resources from domain (`.`) and packages (`.compose/packages/`)
3. Resolve duplicate resources across namespaces by matching build version
4. Build timeline: find commits where each resource version was set
5. Build propagation map: iterate timeline chronologically, find dependents
6. Update resources: compose `original-propagated` version strings

### Concurrency

Worker goroutines (bounded by `runtime.NumCPU()`) process packages in parallel with mutex-protected shared state. The `sync` stdlib package is imported as `async` to avoid name collision with the `pkg/sync` package.

## Linting

Uses golangci-lint v2 with: dupl (threshold: 100), errcheck, goconst, gosec, govet, ineffassign, revive, staticcheck, unused. Formatter: goimports.

## CI

GitHub Actions workflow (`.github/workflows/commit.yml`) runs four jobs: sync with vault integration, basic bump commands, go-linters, go-tests.
