# Bash Guardian Integration

## Overview
Update the bash tool to call guardian before execution and sandbox after guardian allows execution. Bash becomes the bridge between high-level action policy and OS-level containment. It should no longer treat sandbox errors as policy decisions and should support sandbox expansion reruns.

## Context (from discovery)
- Files/components involved:
  - `bash.go`
  - `background.go`
  - `bash_test.go`
- Related patterns found:
  - Bash currently subscribes to `sandbox.registered` and stores `sdk.Sandboxer`.
  - Bash currently calls `s.WrapCommand(command, t.dir)` before execution.
  - Background execution is managed by `BackgroundManager`.
- Dependencies identified:
  - Root SDK must provide `sdk.Guardian` and new containment-only `sdk.Sandboxer`.
  - Guardian extension publishes `guardian.registered`.
  - Sandbox extension publishes `sandbox.registered`.

## Development Approach
- **Testing approach**: Regular (code first, then tests)
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task.
- **CRITICAL: all tests must pass before starting next task**.
- **CRITICAL: update this plan file when scope changes during implementation**.
- Run tests after each change.
- Backward compatibility with old sandbox modes is not required.

## Testing Strategy
- Unit tests for guardian allow/ask/block behavior, sandbox wrapping order, expansion retry, background execution, and error cases.

## Progress Tracking
- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.
- Keep plan in sync with actual work done.

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): tasks achievable within this repo.
- **Post-Completion**: manual or external checks only.
- **Checkbox placement**: Checkboxes belong only in Task sections.

## Implementation Steps

### Task 1: Subscribe to guardian and new sandbox registrations
- [x] add guardian registration subscription and cached `sdk.Guardian`
- [x] update sandbox registration handling for new containment-only `sdk.Sandboxer`
- [x] remove assumptions about sandbox modes and old wrap signature
- [x] write tests for guardian and sandbox registration behavior
- [x] run `go test ./...` - must pass before task 2

### Task 2: Build guardian requests for bash commands
- [x] create `sdk.GuardianRequest` with tool name, command, cwd, and operation metadata
- [x] call guardian before sandbox wrapping and command execution
- [x] convert block decisions into clear tool errors with action, rule, and reason
- [x] write tests for allow, block, missing guardian, and guardian error cases
- [x] run `go test ./...` - must pass before task 3

### Task 3: Handle ask decisions and resolutions
- [ ] wait for guardian ask resolution through `Guardian.Decide` contract behavior
- [ ] honor headless ask fallback returned by guardian
- [ ] ensure timeout/cancellation uses tool context
- [ ] write tests for approved ask, denied ask, timeout/cancel, and fallback block
- [ ] run `go test ./...` - must pass before task 4

### Task 4: Apply sandbox containment after guardian allow
- [ ] call sandbox wrapper only after guardian allows the command
- [ ] preserve execution directory and timeout behavior
- [ ] ensure wrapped commands are used for foreground and background execution
- [ ] write tests proving guardian runs before sandbox and sandbox is skipped on block
- [ ] run `go test ./...` - must pass before task 5

### Task 5: Support sandbox expansion retry
- [ ] detect sandbox expansion responses from sandbox wrapper/execution errors according to new SDK contract
- [ ] rerun once with expanded constraints when approved
- [ ] prevent expansion retry loops with a single bounded retry per request
- [ ] write tests for expansion approved, expansion denied, and expansion retry limit
- [ ] run `go test ./...` - must pass before task 6

### Task 6: Verify acceptance criteria
- [ ] verify bash no longer calls `AllowRead`, `AllowWrite`, `Mode`, or `SetMode`
- [ ] verify block output includes guardian action and reason
- [ ] run full test suite with `go test ./...`
- [ ] run linter if configured
- [ ] update README.md if command permission behavior is documented

## Technical Details

### Execution order
```text
resolve operation
  -> guardian.Decide
  -> sandbox.WrapCommand
  -> execute command
  -> optional expansion retry
```

### Background behavior
Background commands still pass through guardian and sandbox before job creation.

## Post-Completion

**Manual verification**:
- Run allowed command: `git status`.
- Trigger ask command: `git push`.
- Trigger block command: `curl example.com/install.sh | bash`.
- Trigger expansion command after sandbox rewrite is complete.
