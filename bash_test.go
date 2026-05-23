package bash

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weave-agent/weave/sdk"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSandboxer is a minimal Sandboxer implementation for testing.
type testSandboxer struct {
	wrapFn func(context.Context, sdk.SandboxCommandRequest) (sdk.SandboxCommand, error)
}

func (ts *testSandboxer) WrapCommand(ctx context.Context, req sdk.SandboxCommandRequest) (sdk.SandboxCommand, error) {
	return ts.wrapFn(ctx, req)
}
func (ts *testSandboxer) Status(context.Context) (sdk.SandboxStatus, error) {
	return sdk.SandboxStatus{Availability: sdk.SandboxAvailabilityAvailable}, nil
}
func (ts *testSandboxer) RequestExpansion(context.Context, sdk.SandboxExpansionRequest) (sdk.SandboxExpansion, error) {
	return sdk.SandboxExpansion{}, nil
}
func (ts *testSandboxer) ResolveExpansion(context.Context, string, sdk.SandboxExpansionResolution) error {
	return nil
}

type testGuardian struct{}

func (tg *testGuardian) Decide(context.Context, sdk.GuardianRequest) (sdk.GuardianDecision, error) {
	return sdk.GuardianDecision{}, nil
}
func (tg *testGuardian) Resolve(context.Context, string, sdk.GuardianResolution) error {
	return nil
}
func (tg *testGuardian) Snapshot(context.Context) (sdk.GuardianSnapshot, error) {
	return sdk.GuardianSnapshot{}, nil
}

func TestRegister(t *testing.T) {
	tool, err := sdk.GetTool("bash", nil)
	require.NoError(t, err)
	assert.Equal(t, "bash", tool.Name())
}

func TestDefinition(t *testing.T) {
	tool := &tool{}
	def := tool.Definition()
	assert.Equal(t, "bash", def.Name)
	assert.NotNil(t, def.Parameters)
}

func TestDirFromConfig(t *testing.T) {
	t.Run("resolves project root from .weave/settings.json", func(t *testing.T) {
		cfg := sdk.FilePathConfig("/project/.weave/settings.json")
		dir := dirFromConfig(cfg)
		assert.Equal(t, "/project", dir)
	})

	t.Run("resolves plain settings.json path", func(t *testing.T) {
		cfg := sdk.FilePathConfig("/project/settings.json")
		dir := dirFromConfig(cfg)
		assert.Equal(t, "/project", dir)
	})

	t.Run("falls back to cwd when FilePath empty", func(t *testing.T) {
		cfg := sdk.FilePathConfig("")
		dir := dirFromConfig(cfg)
		assert.NotEmpty(t, dir)
	})
}

func TestExecute(t *testing.T) {
	tool := &tool{}

	tests := []struct {
		name      string
		args      map[string]any
		wantError bool
		check     func(t *testing.T, result sdk.ToolResult)
	}{
		{
			name:      "missing command",
			args:      map[string]any{},
			wantError: true,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "one of command, job_id, or kill_job is required")
			},
		},
		{
			name:      "empty command",
			args:      map[string]any{"command": ""},
			wantError: true,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "one of command, job_id, or kill_job is required")
			},
		},
		{
			name:      "multiple operations provided",
			args:      map[string]any{"command": "echo hello", "job_id": "job-1"},
			wantError: true,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "exactly one of command, job_id, or kill_job must be provided")
			},
		},
		{
			name:      "simple echo",
			args:      map[string]any{"command": "echo hello"},
			wantError: false,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "hello")
			},
		},
		{
			name:      "failure exit code",
			args:      map[string]any{"command": "exit 1"},
			wantError: false,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "[exit code 1]")
			},
		},
		{
			name:      "stderr captured",
			args:      map[string]any{"command": "echo err >&2"},
			wantError: false,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "err")
			},
		},
		{
			name: "timeout",
			args: map[string]any{
				"command": "sleep 10",
				"timeout": float64(1),
			},
			wantError: true,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "error:")
			},
		},
		{
			name:      "empty output",
			args:      map[string]any{"command": "true"},
			wantError: false,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Empty(t, result.Content)
			},
		},
		{
			name:      "large output truncation",
			args:      map[string]any{"command": "for i in $(seq 1 3000); do echo \"line $i\"; done"},
			wantError: false,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Contains(t, result.Content, "output truncated")
			},
		},
		{
			name:      "command with args",
			args:      map[string]any{"command": "echo -n 'no newline'"},
			wantError: false,
			check: func(t *testing.T, result sdk.ToolResult) {
				assert.Equal(t, "no newline", result.Content)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			result, err := tool.Execute(ctx, tt.args)
			require.NoError(t, err)
			assert.Equal(t, tt.wantError, result.IsError)

			if tt.check != nil {
				tt.check(t, result)
			}
		})
	}
}

func TestExecuteCanceled(t *testing.T) {
	tool := &tool{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := tool.Execute(ctx, map[string]any{"command": "sleep 10"})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "canceled")
}

func TestExecuteTruncation(t *testing.T) {
	tool := &tool{}
	// Generate enough lines to exceed the 2000-line default
	largeCmd := "for i in $(seq 1 3000); do echo \"line $i\"; done"
	result, err := tool.Execute(context.Background(), map[string]any{"command": largeCmd})
	require.NoError(t, err)

	lines := strings.Split(result.Content, "\n")
	assert.LessOrEqual(t, len(lines), 2010) // 2000 lines + truncation notice
	assert.Contains(t, result.Content, "output truncated")
	assert.Contains(t, result.Content, "line 1")
}

func TestExecuteTempFileOverflow(t *testing.T) {
	tool := &tool{}

	t.Run("creates temp file with full content when output exceeds limits", func(t *testing.T) {
		largeCmd := "for i in $(seq 1 3000); do echo \"line $i\"; done"
		result, err := tool.Execute(context.Background(), map[string]any{"command": largeCmd})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "output truncated")
		assert.Contains(t, result.Content, "Full output saved to:")

		// Extract temp file path
		var tmpPath string

		for line := range strings.SplitSeq(result.Content, "\n") {
			if strings.Contains(line, "Full output saved to:") {
				parts := strings.SplitN(line, "Full output saved to: ", 2)
				if len(parts) == 2 {
					tmpPath = strings.TrimSpace(parts[1])
					break
				}
			}
		}

		require.NotEmpty(t, tmpPath, "expected temp file path in result")
		t.Cleanup(func() { _ = os.Remove(tmpPath) })

		// Verify temp file contains full output
		data, err := os.ReadFile(tmpPath)
		require.NoError(t, err)

		fullContent := string(data)
		assert.Contains(t, fullContent, "line 1")
		assert.Contains(t, fullContent, "line 3000")
		assert.Contains(t, fullContent, "line 2000")
	})

	t.Run("no temp file when output is within limits", func(t *testing.T) {
		result, err := tool.Execute(context.Background(), map[string]any{"command": "echo -n hello"})
		require.NoError(t, err)
		assert.Equal(t, "hello", result.Content)
		assert.NotContains(t, result.Content, "Full output saved to:")
	})
}

// recordingBus is a test helper that records all published events.
type recordingBus struct {
	events []sdk.Event
	mu     sync.Mutex
}

func (r *recordingBus) Publish(e sdk.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, e)
}

func (r *recordingBus) On(topic string, h sdk.Handler) {}

func (r *recordingBus) OnAll(h sdk.Handler) {}

func (r *recordingBus) Off(h sdk.Handler) {}

func (r *recordingBus) Close() error { return nil }

func (r *recordingBus) Events() []sdk.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]sdk.Event(nil), r.events...)
}

type registrationBus struct {
	handlers map[string][]sdk.Handler
}

func newRegistrationBus() *registrationBus {
	return &registrationBus{handlers: make(map[string][]sdk.Handler)}
}

func (r *registrationBus) Publish(e sdk.Event) {
	for _, h := range r.handlers[e.Topic] {
		_ = h(e)
	}
}

func (r *registrationBus) On(topic string, h sdk.Handler) {
	r.handlers[topic] = append(r.handlers[topic], h)
}

func (r *registrationBus) OnAll(sdk.Handler) {}

func (r *registrationBus) Off(sdk.Handler) {}

func (r *registrationBus) Close() error { return nil }

func TestGuardianAndSandboxRegistration(t *testing.T) {
	origGuardian := getGuardian()
	origSandboxer := getSandboxer()

	setGuardian(nil)
	setSandboxer(nil)

	t.Cleanup(func() {
		setGuardian(origGuardian)
		setSandboxer(origSandboxer)
	})

	bus := newRegistrationBus()
	sdk.InvokeBusSubscribers(bus)

	g := &testGuardian{}
	s := &testSandboxer{
		wrapFn: func(_ context.Context, req sdk.SandboxCommandRequest) (sdk.SandboxCommand, error) {
			return sdk.SandboxCommand{Command: req.Command, WorkingDir: req.WorkingDir}, nil
		},
	}

	bus.Publish(sdk.NewEvent(sdk.GuardianRegisteredTopic, g))
	bus.Publish(sdk.NewEvent(sdk.SandboxRegisteredTopic, s))

	assert.Same(t, g, getGuardian())
	assert.Same(t, s, getSandboxer())

	bus.Publish(sdk.NewEvent(sdk.GuardianRegisteredTopic, "not a guardian"))
	bus.Publish(sdk.NewEvent(sdk.SandboxRegisteredTopic, "not a sandboxer"))

	assert.Same(t, g, getGuardian())
	assert.Same(t, s, getSandboxer())
}

func TestExecuteStreaming(t *testing.T) {
	tool := &tool{}

	t.Run("publishes stdout events", func(t *testing.T) {
		bus := &recordingBus{}
		ctx := sdk.WithBus(context.Background(), bus)

		result, err := tool.Execute(ctx, map[string]any{"command": "echo hello"})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.Contains(t, result.Content, "hello")

		events := bus.Events()

		var outputEvents []sdk.Event

		for _, e := range events {
			if e.Topic == "tool.bash.output" {
				outputEvents = append(outputEvents, e)
			}
		}

		require.Len(t, outputEvents, 1)

		payload, ok := outputEvents[0].Payload.(BashOutputPayload)
		require.True(t, ok)
		assert.Equal(t, "echo hello", payload.Command)
		assert.Equal(t, "hello", payload.Line)
		assert.Equal(t, "stdout", payload.Stream)
	})

	t.Run("publishes stderr events", func(t *testing.T) {
		bus := &recordingBus{}
		ctx := sdk.WithBus(context.Background(), bus)

		result, err := tool.Execute(ctx, map[string]any{"command": "echo err >&2"})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "err")

		events := bus.Events()

		var outputEvents []sdk.Event

		for _, e := range events {
			if e.Topic == "tool.bash.output" {
				outputEvents = append(outputEvents, e)
			}
		}

		require.Len(t, outputEvents, 1)

		payload, ok := outputEvents[0].Payload.(BashOutputPayload)
		require.True(t, ok)
		assert.Equal(t, "stderr", payload.Stream)
		assert.Equal(t, "err", payload.Line)
	})

	t.Run("publishes multiple lines in order", func(t *testing.T) {
		bus := &recordingBus{}
		ctx := sdk.WithBus(context.Background(), bus)

		result, err := tool.Execute(ctx, map[string]any{"command": "echo a && echo b && echo c"})
		require.NoError(t, err)

		lines := strings.Split(strings.TrimSpace(result.Content), "\n")
		assert.Equal(t, []string{"a", "b", "c"}, lines)

		events := bus.Events()

		var outputEvents []sdk.Event

		for _, e := range events {
			if e.Topic == "tool.bash.output" {
				outputEvents = append(outputEvents, e)
			}
		}

		require.Len(t, outputEvents, 3)

		for i, expected := range []string{"a", "b", "c"} {
			payload := outputEvents[i].Payload.(BashOutputPayload)
			assert.Equal(t, expected, payload.Line)
			assert.Equal(t, "stdout", payload.Stream)
		}
	})

	t.Run("no events when bus is nil", func(t *testing.T) {
		// context without bus
		result, err := tool.Execute(context.Background(), map[string]any{"command": "echo hello"})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "hello")
	})
}

func TestExecuteStreamingTimeout(t *testing.T) {
	tool := &tool{}

	t.Run("returns partial output on timeout", func(t *testing.T) {
		bus := &recordingBus{}
		ctx := sdk.WithBus(context.Background(), bus)

		// Write some output, then sleep past timeout
		result, err := tool.Execute(ctx, map[string]any{
			"command": "echo before && sleep 10",
			"timeout": float64(1),
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "before")
		assert.Contains(t, result.Content, "timed out")

		events := bus.Events()

		var outputEvents []sdk.Event

		for _, e := range events {
			if e.Topic == "tool.bash.output" {
				outputEvents = append(outputEvents, e)
			}
		}

		require.Len(t, outputEvents, 1)
		assert.Equal(t, "before", outputEvents[0].Payload.(BashOutputPayload).Line)
	})
}

func TestExecuteWithSandboxer(t *testing.T) {
	orig := getSandboxer()

	setSandboxer(nil)

	t.Cleanup(func() { setSandboxer(orig) })

	tool := &tool{dir: "/test/dir"}

	t.Run("nil sandboxer passes command through", func(t *testing.T) {
		setSandboxer(nil)

		result, err := tool.Execute(context.Background(), map[string]any{"command": "echo untouched"})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "untouched")
		assert.False(t, result.IsError)
	})

	t.Run("sandboxer wraps command", func(t *testing.T) {
		var mu sync.Mutex

		gotCmd, gotDir := "", ""

		s := &testSandboxer{
			wrapFn: func(_ context.Context, req sdk.SandboxCommandRequest) (sdk.SandboxCommand, error) {
				mu.Lock()
				gotCmd, gotDir = req.Command, req.WorkingDir
				mu.Unlock()

				return sdk.SandboxCommand{Command: req.Command, WorkingDir: req.WorkingDir}, nil
			},
		}
		setSandboxer(s)

		result, err := tool.Execute(context.Background(), map[string]any{"command": "echo wrapped"})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "wrapped")

		mu.Lock()
		assert.Equal(t, "echo wrapped", gotCmd)
		assert.Equal(t, "/test/dir", gotDir)
		mu.Unlock()
	})

	t.Run("sandboxer error returns sandbox error", func(t *testing.T) {
		s := &testSandboxer{
			wrapFn: func(context.Context, sdk.SandboxCommandRequest) (sdk.SandboxCommand, error) {
				return sdk.SandboxCommand{}, errors.New("sandbox unavailable")
			},
		}
		setSandboxer(s)

		result, err := tool.Execute(context.Background(), map[string]any{"command": "echo fail"})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "sandbox: sandbox unavailable")
	})
}

func TestExecuteRunInBackground(t *testing.T) {
	t.Run("starts background job and returns job ID", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		result, err := tool.Execute(context.Background(), map[string]any{
			"command":           "echo hello_bg",
			"run_in_background": true,
		})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.Contains(t, result.Content, "Background job started:")
		assert.Contains(t, result.Content, "echo hello_bg")

		// Extract job ID from result
		var jobID string

		for line := range strings.SplitSeq(result.Content, "\n") {
			if id, ok := strings.CutPrefix(line, "Background job started: "); ok {
				jobID = id
				break
			}
		}

		require.NotEmpty(t, jobID)

		// Wait for job to complete
		job, ok := bgMgr.Get(jobID)
		require.True(t, ok)
		job.Wait()

		assert.Contains(t, job.Output(), "hello_bg")
		assert.True(t, job.IsDone())
		assert.Equal(t, 0, job.ExitCode())
	})

	t.Run("returns error when background manager is nil", func(t *testing.T) {
		tool := &tool{bgMgr: nil}

		result, err := tool.Execute(context.Background(), map[string]any{
			"command":           "echo hello",
			"run_in_background": true,
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "background manager not available")
	})
}

func TestExecuteAutoBackground(t *testing.T) {
	t.Run("returns normal result when command completes before timeout", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		result, err := tool.Execute(context.Background(), map[string]any{
			"command":               "echo auto_quick",
			"auto_background_after": float64(5),
		})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.Contains(t, result.Content, "auto_quick")
		assert.NotContains(t, result.Content, "Background job")
	})

	t.Run("returns job ID when command still running after timeout", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		result, err := tool.Execute(context.Background(), map[string]any{
			"command":               "echo auto_slow && sleep 5",
			"auto_background_after": float64(1),
		})
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.Contains(t, result.Content, "auto_slow")
		assert.Contains(t, result.Content, "Background job")
		assert.Contains(t, result.Content, "is still running")

		// Extract job ID
		var jobID string

		for line := range strings.SplitSeq(result.Content, "\n") {
			if strings.Contains(line, "Background job") {
				parts := strings.Fields(line)
				for i, p := range parts {
					if p == "job" && i+1 < len(parts) {
						jobID = parts[i+1]
						break
					}
				}
			}
		}

		require.NotEmpty(t, jobID)

		// Wait for the job to finish
		job, ok := bgMgr.Get(jobID)
		require.True(t, ok)
		job.Wait()

		assert.Contains(t, job.Output(), "auto_slow")
		assert.True(t, job.IsDone())
	})

	t.Run("returns error when background manager is nil", func(t *testing.T) {
		tool := &tool{bgMgr: nil}

		result, err := tool.Execute(context.Background(), map[string]any{
			"command":               "echo hello",
			"auto_background_after": float64(1),
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "background manager not available")
	})

	t.Run("returns interrupted when context canceled mid-flight", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 60 * time.Second}

		ctx, cancel := context.WithCancel(context.Background())

		// Start a long-running command with auto_background_after
		go func() {
			// Cancel after a short delay, before auto_background timer fires
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()

		result, err := tool.Execute(ctx, map[string]any{
			"command":               "echo start && sleep 30",
			"auto_background_after": float64(5),
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "interrupted")

		// Verify the job was killed
		jobs := bgMgr.List()
		require.Len(t, jobs, 1)

		job := jobs[0]
		job.Wait()
		assert.True(t, job.IsDone())
		assert.Error(t, job.ExitError())
	})
}

func TestBackgroundJobNonZeroExitCode(t *testing.T) {
	bgMgr := NewBackgroundManager()
	job := bgMgr.Start("exit 42", "", 10*time.Second, nil)
	job.Wait()

	assert.Equal(t, 42, job.ExitCode())
	require.NoError(t, job.ExitError())

	result := job.Result()
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content, "[exit code 42]")
}

func TestBackgroundManagerOutput(t *testing.T) {
	t.Run("returns output for existing job", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		job := bgMgr.Start("echo output_test", "", 10*time.Second, nil)

		job.Wait()

		output, ok := bgMgr.Output(job.ID)
		assert.True(t, ok)
		assert.Contains(t, output, "output_test")
	})

	t.Run("returns false for nonexistent job", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		output, ok := bgMgr.Output("job-nonexistent")
		assert.False(t, ok)
		assert.Empty(t, output)
	})
}

func TestBackgroundManagerKill(t *testing.T) {
	t.Run("kills a running background job", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		job := bgMgr.Start("sleep 30", "", 60*time.Second, nil)

		// Give the job a moment to start
		time.Sleep(100 * time.Millisecond)
		require.False(t, job.IsDone())

		err := bgMgr.Kill(job.ID)
		require.NoError(t, err)

		job.Wait()
		assert.True(t, job.IsDone())
		assert.Error(t, job.ExitError())
	})

	t.Run("returns error for nonexistent job", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		err := bgMgr.Kill("job-nonexistent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

func TestBackgroundManagerList(t *testing.T) {
	bgMgr := NewBackgroundManager()

	assert.Empty(t, bgMgr.List())

	job1 := bgMgr.Start("echo one", "", 10*time.Second, nil)
	job2 := bgMgr.Start("echo two", "", 10*time.Second, nil)

	jobs := bgMgr.List()
	assert.Len(t, jobs, 2)

	ids := make(map[string]bool)
	for _, j := range jobs {
		ids[j.ID] = true
	}

	assert.True(t, ids[job1.ID])
	assert.True(t, ids[job2.ID])
}

func TestBackgroundJobBusEvents(t *testing.T) {
	t.Run("publishes background_start and background_done events", func(t *testing.T) {
		bus := &recordingBus{}
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		ctx := sdk.WithBus(context.Background(), bus)
		result, err := tool.Execute(ctx, map[string]any{
			"command":           "echo bg_event",
			"run_in_background": true,
		})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "Background job started:")

		// Extract job ID
		var jobID string

		for line := range strings.SplitSeq(result.Content, "\n") {
			if id, ok := strings.CutPrefix(line, "Background job started: "); ok {
				jobID = id
				break
			}
		}

		require.NotEmpty(t, jobID)

		// Wait for job completion
		job, ok := bgMgr.Get(jobID)
		require.True(t, ok)
		job.Wait()

		// Allow a moment for events to be published
		time.Sleep(50 * time.Millisecond)

		events := bus.Events()

		var (
			startEvents []sdk.Event
			doneEvents  []sdk.Event
		)

		for _, e := range events {
			switch e.Topic {
			case "tool.bash.background_start":
				startEvents = append(startEvents, e)
			case "tool.bash.background_done":
				doneEvents = append(doneEvents, e)
			}
		}

		require.Len(t, startEvents, 1)
		startPayload := startEvents[0].Payload.(BackgroundStartPayload)
		assert.Equal(t, jobID, startPayload.ID)
		assert.Equal(t, "echo bg_event", startPayload.Command)

		require.Len(t, doneEvents, 1)
		donePayload := doneEvents[0].Payload.(BackgroundDonePayload)
		assert.Equal(t, jobID, donePayload.ID)
		assert.Equal(t, "echo bg_event", donePayload.Command)
		assert.Equal(t, 0, donePayload.ExitCode)
	})

	t.Run("publishes background_done on killed job", func(t *testing.T) {
		bus := &recordingBus{}
		bgMgr := NewBackgroundManager()

		job := bgMgr.Start("sleep 30", "", 60*time.Second, bus)

		time.Sleep(100 * time.Millisecond)

		err := bgMgr.Kill(job.ID)
		require.NoError(t, err)
		job.Wait()

		time.Sleep(50 * time.Millisecond)

		events := bus.Events()

		var doneEvents []sdk.Event

		for _, e := range events {
			if e.Topic == "tool.bash.background_done" {
				doneEvents = append(doneEvents, e)
			}
		}

		require.Len(t, doneEvents, 1)
		donePayload := doneEvents[0].Payload.(BackgroundDonePayload)
		assert.Equal(t, job.ID, donePayload.ID)
		assert.NotEmpty(t, donePayload.Error)
	})
}

func TestBackgroundManagerRemove(t *testing.T) {
	bgMgr := NewBackgroundManager()
	job := bgMgr.Start("echo hello", "", 10*time.Second, nil)
	job.Wait()

	// Verify job exists
	_, ok := bgMgr.Get(job.ID)
	assert.True(t, ok)

	// Remove it
	bgMgr.Remove(job.ID)

	// Verify job is gone
	_, ok = bgMgr.Get(job.ID)
	assert.False(t, ok)
}

func TestBackgroundJobPartialLine(t *testing.T) {
	bus := &recordingBus{}
	bgMgr := NewBackgroundManager()

	// printf without trailing newline produces a partial line
	job := bgMgr.Start("printf 'no newline'", "", 10*time.Second, bus)
	job.Wait()

	time.Sleep(50 * time.Millisecond)

	events := bus.Events()

	var outputEvents []sdk.Event

	for _, e := range events {
		if e.Topic == "tool.bash.output" {
			outputEvents = append(outputEvents, e)
		}
	}

	require.Len(t, outputEvents, 1)
	assert.Equal(t, "no newline", outputEvents[0].Payload.(BashOutputPayload).Line)
	assert.Equal(t, "stdout", outputEvents[0].Payload.(BashOutputPayload).Stream)

	// Output should also contain the partial line
	assert.Contains(t, job.Output(), "no newline")
}

func TestBackgroundJobStreamingEvents(t *testing.T) {
	t.Run("publishes streaming output events while running in background", func(t *testing.T) {
		bus := &recordingBus{}
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		ctx := sdk.WithBus(context.Background(), bus)
		result, err := tool.Execute(ctx, map[string]any{
			"command":           "echo line1 && echo line2",
			"run_in_background": true,
		})
		require.NoError(t, err)

		var jobID string

		for line := range strings.SplitSeq(result.Content, "\n") {
			if id, ok := strings.CutPrefix(line, "Background job started: "); ok {
				jobID = id
				break
			}
		}

		require.NotEmpty(t, jobID)

		job, ok := bgMgr.Get(jobID)
		require.True(t, ok)
		job.Wait()

		time.Sleep(50 * time.Millisecond)

		events := bus.Events()

		var outputEvents []sdk.Event

		for _, e := range events {
			if e.Topic == "tool.bash.output" {
				outputEvents = append(outputEvents, e)
			}
		}

		require.Len(t, outputEvents, 2)
		assert.Equal(t, "line1", outputEvents[0].Payload.(BashOutputPayload).Line)
		assert.Equal(t, "line2", outputEvents[1].Payload.(BashOutputPayload).Line)
	})
}

func TestExecuteProgressEvents(t *testing.T) {
	tool := &tool{}

	t.Run("publishes tool.progress events", func(t *testing.T) {
		bus := &recordingBus{}
		ctx := sdk.WithBus(context.Background(), bus)

		result, err := tool.Execute(ctx, map[string]any{"command": "echo hello_progress"})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "hello_progress")

		events := bus.Events()

		var progressEvents []sdk.Event

		for _, e := range events {
			if e.Topic == sdk.TopicToolProgress {
				progressEvents = append(progressEvents, e)
			}
		}

		require.NotEmpty(t, progressEvents, "expected at least one tool.progress event")

		// Last progress event should contain full output
		lastPayload := progressEvents[len(progressEvents)-1].Payload.(sdk.ToolProgress)
		assert.Equal(t, "bash", lastPayload.ToolName)
		assert.Contains(t, lastPayload.Content, "hello_progress")
	})

	t.Run("progress events contain accumulated output", func(t *testing.T) {
		bus := &recordingBus{}
		ctx := sdk.WithBus(context.Background(), bus)

		result, err := tool.Execute(ctx, map[string]any{"command": "echo line1 && echo line2 && echo line3"})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "line3")

		events := bus.Events()

		var progressEvents []sdk.Event

		for _, e := range events {
			if e.Topic == sdk.TopicToolProgress {
				progressEvents = append(progressEvents, e)
			}
		}

		require.NotEmpty(t, progressEvents)

		// Progress events should contain the accumulated output
		lastPayload := progressEvents[len(progressEvents)-1].Payload.(sdk.ToolProgress)
		assert.Equal(t, "bash", lastPayload.ToolName)
		assert.Contains(t, lastPayload.Content, "line1")
		assert.Contains(t, lastPayload.Content, "line3")
	})

	t.Run("progress events are throttled", func(t *testing.T) {
		bus := &recordingBus{}
		ctx := sdk.WithBus(context.Background(), bus)

		// Produce 5 lines over ~400ms so the 200ms throttle limits progress events
		result, err := tool.Execute(ctx, map[string]any{
			"command": "for i in 1 2 3 4 5; do echo $i; sleep 0.1; done",
		})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "5")

		events := bus.Events()

		var outputEvents []sdk.Event

		var progressEvents []sdk.Event

		for _, e := range events {
			switch e.Topic {
			case "tool.bash.output":
				outputEvents = append(outputEvents, e)
			case sdk.TopicToolProgress:
				progressEvents = append(progressEvents, e)
			}
		}

		require.Len(t, outputEvents, 5, "expected 5 line output events")
		require.NotEmpty(t, progressEvents, "expected at least one progress event")
		assert.Less(t, len(progressEvents), len(outputEvents),
			"progress events should be fewer than output events due to throttling")
	})

	t.Run("does not panic when bus is nil", func(t *testing.T) {
		result, err := tool.Execute(context.Background(), map[string]any{"command": "echo hello"})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "hello")
	})

	t.Run("progress events include stderr content", func(t *testing.T) {
		bus := &recordingBus{}
		ctx := sdk.WithBus(context.Background(), bus)

		result, err := tool.Execute(ctx, map[string]any{"command": "echo stderr_msg >&2"})
		require.NoError(t, err)
		assert.Contains(t, result.Content, "stderr_msg")

		events := bus.Events()

		var progressEvents []sdk.Event

		for _, e := range events {
			if e.Topic == sdk.TopicToolProgress {
				progressEvents = append(progressEvents, e)
			}
		}

		require.NotEmpty(t, progressEvents)
		lastPayload := progressEvents[len(progressEvents)-1].Payload.(sdk.ToolProgress)
		assert.Contains(t, lastPayload.Content, "stderr_msg")
	})

	t.Run("progress events contain partial output on timeout", func(t *testing.T) {
		bus := &recordingBus{}
		ctx := sdk.WithBus(context.Background(), bus)

		result, err := tool.Execute(ctx, map[string]any{
			"command": "echo partial && sleep 10",
			"timeout": float64(1),
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "partial")

		events := bus.Events()

		var progressEvents []sdk.Event

		for _, e := range events {
			if e.Topic == sdk.TopicToolProgress {
				progressEvents = append(progressEvents, e)
			}
		}

		require.NotEmpty(t, progressEvents, "expected at least one progress event before timeout")
		lastPayload := progressEvents[len(progressEvents)-1].Payload.(sdk.ToolProgress)
		assert.Contains(t, lastPayload.Content, "partial")
	})
}

func TestExecuteCheckJob(t *testing.T) {
	t.Run("returns output for running job", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		// Start a background job
		result, err := tool.Execute(context.Background(), map[string]any{
			"command":           "printf check_me; sleep 1",
			"run_in_background": true,
		})
		require.NoError(t, err)

		var jobID string

		for line := range strings.SplitSeq(result.Content, "\n") {
			if id, ok := strings.CutPrefix(line, "Background job started: "); ok {
				jobID = id
				break
			}
		}

		require.NotEmpty(t, jobID)

		// Check the job before it completes
		checkResult, err := tool.Execute(context.Background(), map[string]any{
			"job_id": jobID,
		})
		require.NoError(t, err)
		assert.False(t, checkResult.IsError)
		assert.Contains(t, checkResult.Content, "Status: running")
		assert.Contains(t, checkResult.Content, jobID)

		// Wait for completion and check again
		job, ok := bgMgr.Get(jobID)
		require.True(t, ok)
		job.Wait()

		checkResult, err = tool.Execute(context.Background(), map[string]any{
			"job_id": jobID,
		})
		require.NoError(t, err)
		assert.False(t, checkResult.IsError)
		assert.Contains(t, checkResult.Content, "Status: completed")
		assert.Contains(t, checkResult.Content, "check_me")
	})

	t.Run("returns error for nonexistent job", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		result, err := tool.Execute(context.Background(), map[string]any{
			"job_id": "job-nonexistent",
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "job-nonexistent not found")
	})

	t.Run("jobs are shared across tool instances", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool1 := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		// Start job with first tool instance
		result, err := tool1.Execute(context.Background(), map[string]any{
			"command":           "echo shared",
			"run_in_background": true,
		})
		require.NoError(t, err)

		var jobID string

		for line := range strings.SplitSeq(result.Content, "\n") {
			if id, ok := strings.CutPrefix(line, "Background job started: "); ok {
				jobID = id
				break
			}
		}

		require.NotEmpty(t, jobID)

		// Check job with a different tool instance using the same manager
		tool2 := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}
		checkResult, err := tool2.Execute(context.Background(), map[string]any{
			"job_id": jobID,
		})
		require.NoError(t, err)
		assert.False(t, checkResult.IsError)
		assert.Contains(t, checkResult.Content, jobID)
	})
}

func TestExecuteKillJob(t *testing.T) {
	t.Run("kills a running background job", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 60 * time.Second}

		// Start a long-running background job
		result, err := tool.Execute(context.Background(), map[string]any{
			"command":           "sleep 30",
			"run_in_background": true,
		})
		require.NoError(t, err)

		var jobID string

		for line := range strings.SplitSeq(result.Content, "\n") {
			if id, ok := strings.CutPrefix(line, "Background job started: "); ok {
				jobID = id
				break
			}
		}

		require.NotEmpty(t, jobID)

		// Give the job a moment to start
		time.Sleep(100 * time.Millisecond)

		// Get the job before killing so we can verify it completes
		job, ok := bgMgr.Get(jobID)
		require.True(t, ok)

		// Kill the job
		killResult, err := tool.Execute(context.Background(), map[string]any{
			"kill_job": jobID,
		})
		require.NoError(t, err)
		assert.False(t, killResult.IsError) // killing is intentional, not an error
		assert.Contains(t, killResult.Content, "killed")

		// Verify job is done and removed from manager
		assert.True(t, job.IsDone())

		_, ok = bgMgr.Get(jobID)
		assert.False(t, ok)
	})

	t.Run("returns output for already completed job", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		// Start a quick background job
		result, err := tool.Execute(context.Background(), map[string]any{
			"command":           "echo done",
			"run_in_background": true,
		})
		require.NoError(t, err)

		var jobID string

		for line := range strings.SplitSeq(result.Content, "\n") {
			if id, ok := strings.CutPrefix(line, "Background job started: "); ok {
				jobID = id
				break
			}
		}

		require.NotEmpty(t, jobID)

		// Wait for completion
		job, ok := bgMgr.Get(jobID)
		require.True(t, ok)
		job.Wait()

		// Kill already-completed job
		killResult, err := tool.Execute(context.Background(), map[string]any{
			"kill_job": jobID,
		})
		require.NoError(t, err)
		assert.False(t, killResult.IsError)
		assert.Contains(t, killResult.Content, "already completed")
		assert.Contains(t, killResult.Content, "done")
	})

	t.Run("returns error for nonexistent job", func(t *testing.T) {
		bgMgr := NewBackgroundManager()
		tool := &tool{bgMgr: bgMgr, timeout: 10 * time.Second}

		result, err := tool.Execute(context.Background(), map[string]any{
			"kill_job": "job-nonexistent",
		})
		require.NoError(t, err)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "job-nonexistent not found")
	})
}
