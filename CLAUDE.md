# Bash Extension Notes

## Execution Order

Bash command operations run in this order:

1. Resolve the requested operation.
2. Call `guardian.Decide` for new `command` executions.
3. Call `sandbox.WrapCommand` after guardian allows.
4. If wrapping requests sandbox expansion, call `RequestExpansion` and re-wrap once before starting the process.
5. Execute the wrapped command.

`job_id` and `kill_job` operate on existing background jobs and do not start new guardian or sandbox checks.

## SDK Integration

Use `sdk.Sandboxer.WrapCommand(ctx, sdk.SandboxCommandRequest)` and `RequestExpansion`; do not call old `AllowRead`, `AllowWrite`, `Mode`, or `SetMode` APIs.

When honoring an `sdk.SandboxCommand`, preserve `Command`, `Args`, `Env`, and `WorkingDir` for both foreground and background execution.
