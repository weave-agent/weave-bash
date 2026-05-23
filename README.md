# weave-bash

Bash tool extension for [weave](https://github.com/weave-agent/weave) — executes bash commands with streaming output, background execution, and sandbox integration.

## Fork & Customize

1. Fork this repo
2. Edit the tool implementation (`bash.go`, `background.go`)
3. Install your fork: `weave install github.com/<you>/weave-bash --name bash`

The `--name bash` ensures your fork shadows the official `bash` tool.

## Install

```bash
weave install github.com/weave-agent/weave-bash --name bash
```

## Features

- **Synchronous execution** — run commands and wait for output
- **Background execution** — start long-running commands with `run_in_background`
- **Auto-background** — start synchronously and automatically move to background after N seconds with `auto_background_after`
- **Streaming output** — `tool.bash.output` events for each line of stdout/stderr
- **Progress events** — generic `tool.progress` events with accumulated output (throttled at 200ms) for integration with TUI progress displays
- **Guardian and sandbox integration** — commands are checked by guardian before active sandbox containment is applied

## Parameters

| Parameter | Type | Description |
|-----------|------|-------------|
| `command` | string | The bash command to execute |
| `timeout` | number | Timeout in seconds (default: 120) |
| `run_in_background` | boolean | Run the command in the background and return a job ID immediately |
| `auto_background_after` | number | Start command synchronously and move to background after N seconds if still running. 0 disables auto-background. If the context is canceled while waiting, the job is killed and `"interrupted"` is returned |
| `job_id` | string | Check the output and status of an existing background job |
| `kill_job` | string | Kill a background job by ID |

## Bus Events

During synchronous command execution, the bash tool publishes:

- **`tool.bash.output`** — one event per line of stdout/stderr, with payload `{"command": "...", "line": "...", "stream": "stdout" \| "stderr"}`
- **`tool.progress`** — throttled events (200ms interval) containing the accumulated output so far, with payload `{"tool_name": "bash", "content": "..."}`. This allows generic progress UIs to display bash output without special-casing the tool.

Background jobs additionally publish:

- **`tool.bash.background_start`** — when a background job starts
- **`tool.bash.background_done`** — when a background job completes or is killed

## Development

```bash
git clone git@github.com:weave-agent/weave-bash.git
cd weave-bash

# Add temporary replace for local SDK (don't commit this)
echo 'replace github.com/weave-agent/weave => /path/to/local/weave' >> go.mod

go test ./...
go vet ./...
```

## License

Same as the main weave project.
