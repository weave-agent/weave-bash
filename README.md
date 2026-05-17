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

## Development

```bash
git clone git@github.com:weave-agent/weave-bash.git
cd weave-bash

# Add temporary replace for local SDK (don't commit this)
echo 'replace github.com/weave-agent/weave => /path/to/local/weave' >> go.mod

go test ./...
```

## License

Same as the main weave project.
