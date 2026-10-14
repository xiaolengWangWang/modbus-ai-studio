# Versioned Builds and Platform Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep each release's build files, emit four non-Windows difference documents for 2.0.0, and support patch releases from a version maintenance branch.

**Architecture:** Keep the existing Go source and shared build scripts in place. Write build output to `dist/vX.Y.Z/`; render two reviewed platform comparison sources into four package-specific Markdown assets; validate and publish them with the installers. Release tooling follows the current `main` or matching maintenance branch instead of assuming `main`.

**Tech Stack:** Go, POSIX shell, GitHub Actions, Markdown, Git.

**Spec:** `docs/superpowers/specs/2026-10-14-versioned-builds-platform-parity-design.md`

## Global Constraints

- Target release version: `2.0.0` for every build; compare both Windows standard desktop and Windows Compat, recording Compat-specific capabilities separately.
- Parity assets exist only for macOS Intel, macOS Apple Silicon, Linux Web x64, and Linux Web ARM64. Windows and `modbus-cli` get none.
- Keep `cmd/`, `internal/`, `platform/`, `assets/`, `tests/`, and `build/` in place. Keep build output ignored by Git.
- Existing `dist/` 1.5.0 files must survive the migration; new releases never prune older versions.
- A maintenance release from `maintenance/v2.0` must not update either remote's `main`.
- Do not publish a release as part of this implementation.

## Review Focus

1. `VERSION` differs from `cmd/modbus-ai/main.go`: all build scripts fail before producing mislabeled output (Task 1).
2. A target `dist/v1.5.0/` file already exists with different bytes: local migration stops before moving anything (Task 5).
3. A parity Markdown asset is missing, duplicated, encrypted, has an unfilled placeholder, or names another version/package: prepare and publish reject it (Task 3).
4. A maintenance branch's version does not match its `major.minor`, or CI targets another ref: release preparation rejects it (Task 4).
5. Extra Markdown assets cannot confuse desktop update detection or bypass manifest path/checksum validation (Task 3).

---

### Task 1: Versioned output in build and smoke scripts

**Files:** Modify `build/cli.sh`, `build/linux-web.sh`, `build/macos.sh`, `build/windows.sh`, `build/smoke.sh`, `.github/workflows/package.yml`.

**Interfaces:** Each script uses the version in `cmd/modbus-ai/main.go`, accepts only an equal `VERSION` override, and sets `DIST=dist/v$VERSION`. CI uploads and downloads files under that same path; package filenames remain unchanged.

- [ ] **Step 1: Record the failing behavior.** Run `VERSION=9.9.9 sh build/cli.sh`; it currently accepts a mismatched version. Stop once that is observed; do not keep the generated files.
- [ ] **Step 2: Change the five scripts and CI paths.** Read the source version once per script, reject a different override, and use `dist/v$VERSION`. Change the CI upload globs and smoke download destination to that version directory.
- [ ] **Step 3: Verify.** Run `sh -n build/{cli,linux-web,macos,windows,smoke}.sh`; run `VERSION=9.9.9 sh build/cli.sh` and require a nonzero exit before build; run `sh build/cli.sh` and require the five CLI packages under `dist/v1.5.0/` while the source version remains 1.5.0, none newly created in flat `dist/`.
- [ ] **Step 4: Commit.** Commit only these scripts and the workflow.

### Task 2: Release records and retention by version

**Files:** Modify `build/release/main.go`, `build/release/release_test.go`, `docs/development.md`.

**Interfaces:** `open()` sets `release.dist` to `<repo>/dist/v<version>`. `release.file()` and existing artifact functions continue to use `release.dist`. `clean()` removes only known temporary staging files inside the selected version directory; `publish()` does not delete historical packages.

- [ ] **Step 1: Add failing tests.** Replace `TestCleanRemovesOlderVersionsOnly` with `TestCleanPreservesVersionedAssets`; create sibling `v1.5.0` and `v2.0.0` directories, invoke `clean()` for 2.0.0, and assert both versions' installers, checksums, and release records remain. Add `TestReleaseDistUsesVersionDirectory`: create a temporary repo-shaped directory with `cmd/modbus-ai/main.go` declaring 2.0.0, call `open()` from it, and assert `release.dist` is `dist/v2.0.0`.
- [ ] **Step 2: Run `go test ./build/release -run 'TestCleanPreservesVersionedAssets|TestReleaseDistUsesVersionDirectory' -count=1` and confirm red.**
- [ ] **Step 3: Make the smallest release-tool change.** Set the version directory in `open()`; limit `clean()` to current-version temporary staging; update path comments and the development guide. Do not add a cleanup policy for other versions.
- [ ] **Step 4: Run the targeted tests, then `go test ./build/release`.** Require both to pass.
- [ ] **Step 5: Commit.** Commit the release-tool and guide changes.

### Task 3: Four platform-difference release assets

**Files:** Create `docs/platform-parity/macos.md`, `docs/platform-parity/linux-web.md`; modify `build/macos.sh`, `build/linux-web.sh`, `.github/workflows/package.yml`, `build/release/check.go`, `build/release/release_test.go`, `README.md`, `docs/development.md`.

**Interfaces:** Templates use exactly `{{VERSION}}` and `{{PACKAGE}}`; the review date is maintained in each template. Rendered files start with `# 与 Windows 桌面版的功能差异`, then include `目标版本：<version>`, `Windows 基准版本：<version>`, `对应安装包：<package>`, and `检查日期：YYYY-MM-DD`. `parityDocs(version string) []string` returns four `<package filename>-differences.md` names. `releaseAssets(version string) []string` returns `packages(version)` plus those four names. `checkParityDoc(path, version, packageName string) error` validates plaintext UTF-8, these fields, the decision column, and absence of placeholders. `checkInstallers()` checks packages as before, then extracts/checks four Markdown files and appends their size/SHA-256 to the manifest.

- [ ] **Step 1: Add failing release tests.** Test exactly four names, no CLI/Windows parity file, a valid document, wrong version/package, missing/duplicate zip entry, encrypted `E-SafeNet` content, NUL bytes, placeholder text, invalid UTF-8, and altered parity checksum in `load()`. Extend `preparedRelease()` with four valid Markdown assets.
- [ ] **Step 2: Run `go test ./build/release -run 'TestParity|TestLoad' -count=1` and confirm red.**
- [ ] **Step 3: Write reviewed source documents.** Verify each row against `internal/ai/credentials_{windows,other}.go`, `platform/windows.go`, `internal/ui/`, `internal/web/`, and `internal/update/`. Record Windows-only capabilities, target capabilities, reason, `补齐`/`暂缓`/`不适用` decision, and target-only capabilities. Describe shared core capabilities as present on both.
- [ ] **Step 4: Render and package Markdown.** macOS and Linux Web scripts fill the two placeholders into `dist/v$VERSION/` next to each of their two installers. CI uploads these four files. Release checks add them to the expected asset list, checksums, GitHub/Gitee upload and later verification without changing installer suffixes or update selection.
- [ ] **Step 5: Run targeted tests, `go test ./build/release`, shell syntax checks, and a local render check for all four names and fields.**
- [ ] **Step 6: Commit.** Commit the two sources, script/workflow/release changes, and user-facing directory guidance.

### Task 4: Release from a matching maintenance branch

**Files:** Modify `build/release/main.go`, `build/release/remote.go`, `build/release/release_test.go`.

**Interfaces:** Add `branch string` to `release`, determined from `git branch --show-current` in `open()`. Accept `main` or `maintenance/v<major>.<minor>` matching `release.version`; reject detached HEAD and unrelated branches. `checkPushed()` checks `origin/<branch>`, `waitCI()` dispatches `package.yml` with that branch, and `pushGitee()` pushes the commit to the same named branch plus its tag.

- [ ] **Step 1: Add failing tests.** `TestReleaseBranchForVersion` covers `main` with 2.0.0, matching `maintenance/v2.0` with 2.0.1, mismatched `maintenance/v1.5`, feature branch, and detached HEAD. Use a local HTTP test server to assert `checkPushed()` queries the selected branch and CI dispatch uses it; assert the Gitee ref construction is `refs/heads/maintenance/v2.0`, not `main`.
- [ ] **Step 2: Run only those tests and confirm red.**
- [ ] **Step 3: Implement the branch selection and replace the three hard-coded `main` refs.** Keep tag and release version checks intact. Update the temporary-repo fixture in `TestReleaseDistUsesVersionDirectory` so `open()` sees a named Git branch.
- [ ] **Step 4: Run the targeted tests and `go test ./build/release`.**
- [ ] **Step 5: Commit.** Commit branch-aware release code and tests.

### Task 5: 2.0.0 version, existing artifacts, and final verification

**Files:** Modify `cmd/modbus-ai/main.go`, `platform/windows/README.txt`, `README.md`, `docs/development.md`; move only version-named local artifacts under `/Users/xiaoleng/Desktop/modbus-ai-studio/dist/`.

**Interfaces:** `cmd/modbus-ai/main.go` declares `2.0.0`. The Windows README has a 2.0.0 entry. Documentation describes core source, shared build scripts, `dist/vX.Y.Z/`, platform parity files, `main` feature work, and maintenance patch branches.

- [ ] **Step 1: Set `2.0.0` and update the Windows README and directory/release instructions.** Keep the historical 1.5.0 changelog and other past-version references.
- [ ] **Step 2: Preflight the local 1.5.0 migration.** Enumerate only `dist/` entries whose basename contains `1.5.0`; for every target in `dist/v1.5.0/`, compare bytes (or directory contents) and stop without moving if any target differs. Then move eligible entries; leave unnamed local auxiliary files untouched. Verify file count and SHA-256 before/after.
- [ ] **Step 3: Run `go test ./build/release`, `sh -n build/{cli,linux-web,macos,windows,smoke}.sh`, `go test ./...`, and actual 2.0.0 CLI, macOS, and Linux Web builds on this Mac.** Report the known UI layout test failure separately if it persists; do not claim the full suite passed. Run Windows packaging locally only if its cross compiler is installed; otherwise leave that platform to the release-CI gate.
- [ ] **Step 4: Verify Git state.** Require the feature worktree to contain only intended commits and the original extracted worktree's tracked/untracked status to match the pre-migration baseline. Confirm old `dist/v1.5.0/` files remain and four 2.0.0 parity Markdown files have valid versions and package names.
- [ ] **Step 5: Commit final version and documentation changes.** Report the feature branch and test results; do not tag or publish 2.0.0.
