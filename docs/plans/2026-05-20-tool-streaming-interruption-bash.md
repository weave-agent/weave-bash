# Tool Streaming & Interruption — Bash Extension

## Overview
Fix context cancellation in the auto-background path. Optionally align bash's existing line-streaming with the generic `tool.progress` event model so the TUI doesn't need to special-case bash.

## Context
- `bash.go` — `executeSync` already uses `exec.CommandContext` and handles `ctx.Done()`
- `bash.go` — `collectStream` already publishes `tool.bash.output` line-by-line (lines 436-440)
- `bash.go` — auto-background path (`Execute` lines 236-261) has no `ctx.Done()` check in its `select`
- `bash.go` — `executeSync` has `cmd.Cancel` that kills the process group on context cancel

## Development Approach
- Regular approach
- Every task includes tests before moving to next

## Implementation Steps

### Task 1: Fix context cancellation in auto-background path
- [x] Add `case <-ctx.Done():` to the `select` at line 246
- [x] On cancel: kill the background job via `t.bgMgr.Kill(job.ID)` and return interrupted result
- [x] Write tests: auto-background command gets canceled mid-flight
- [x] Run extension tests — must pass

### Task 2: Publish generic tool.progress events
- [x] In `collectStream`, also publish `tool.progress` events (in addition to `tool.bash.output`)
- [x] Content: accumulated output so far (same as what `syncWriter` has)
- [x] Use `sdk.Throttle` at 200ms to avoid flooding
- [x] Write tests: verify `tool.progress` events are throttled and contain partial output
- [x] Run extension tests — must pass

### Task 3: Verify integration
- [x] Run `go test ./...` in bash extension dir
- [x] Run `make lint` if available (no Makefile, ran `go vet ./...` instead)

## Technical Details

```go
// In Execute, auto-background path:
select {
case <-job.done:
    return job.Result(), nil
case <-timer.C:
    if job.IsDone() { return job.Result(), nil }
    // ... background transition ...
case <-ctx.Done():
    _ = t.bgMgr.Kill(job.ID)
    return sdk.ToolResult{Content: "interrupted", IsError: true}, nil
}
```

## Post-Completion
- Depends on core SDK `Throttle` helper
- TUI can treat bash the same as grep for progress display
- Manual verification: run a slow command with `auto_background_after: 60` and press ESC
