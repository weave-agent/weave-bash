package bash

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/weave-agent/weave/sdk"
	"github.com/weave-agent/weave/utils/truncate"
)

const defaultTimeout = 120 * time.Second

// ParamCommand is the tool parameter name for the command to execute.
const ParamCommand = "command"

// BashOutputPayload is the payload for tool.bash.output bus events.
type BashOutputPayload struct {
	Command string `json:"command"`
	Line    string `json:"line"`
	Stream  string `json:"stream"` // "stdout" or "stderr"
}

// BashConfig holds per-tool settings for the bash tool.
type BashConfig struct {
	Timeout int `json:"timeout" default:"120" env:"TIMEOUT"`
}

type tool struct {
	timeout time.Duration
	dir     string
	bgMgr   *BackgroundManager
}

type executionPlan struct {
	command      string
	args         []string
	dir          string
	env          []string
	direct       bool
	expansionReq *sdk.SandboxExpansionRequest
}

type sandboxExpansionRequestError interface {
	SandboxExpansionRequest() sdk.SandboxExpansionRequest
}

// defaultBgMgr is the shared background manager across all bash tool instances.
// Without a singleton, jobs started by one tool instance are unreachable by
// subsequent calls because sdk.GetTool creates a fresh instance per call.
var defaultBgMgr = NewBackgroundManager()

var (
	sandboxerMu sync.RWMutex
	sandboxer   sdk.Sandboxer
	guardianMu  sync.RWMutex
	guardian    sdk.Guardian
	requestSeq  atomic.Uint64
)

func setSandboxer(s sdk.Sandboxer) {
	sandboxerMu.Lock()
	sandboxer = s
	sandboxerMu.Unlock()
}

func getSandboxer() sdk.Sandboxer {
	sandboxerMu.RLock()

	s := sandboxer

	sandboxerMu.RUnlock()

	return s
}

func setGuardian(g sdk.Guardian) {
	guardianMu.Lock()
	guardian = g
	guardianMu.Unlock()
}

func getGuardian() sdk.Guardian {
	guardianMu.RLock()

	g := guardian

	guardianMu.RUnlock()

	return g
}

//nolint:gochecknoinits // SDK tool registration requires init
func init() {
	sdk.OnBusReady(func(bus sdk.Bus) {
		bus.On(sdk.GuardianRegisteredTopic, func(ev sdk.Event) error {
			if g, ok := ev.Payload.(sdk.Guardian); ok {
				setGuardian(g)
			}

			return nil
		})

		bus.On(sdk.SandboxRegisteredTopic, func(ev sdk.Event) error {
			if s, ok := ev.Payload.(sdk.Sandboxer); ok {
				setSandboxer(s)
			}

			return nil
		})
	})

	sdk.RegisterTool("bash", func(cfg sdk.Config, _ sdk.PreferenceReader, bc BashConfig) (sdk.Tool, error) {
		timeout := time.Duration(bc.Timeout) * time.Second
		if timeout <= 0 {
			timeout = defaultTimeout
		}

		dir := dirFromConfig(cfg)

		return &tool{timeout: timeout, dir: dir, bgMgr: defaultBgMgr}, nil
	})
}

func dirFromConfig(cfg sdk.Config) string {
	if pd := cfg.ProjectDir(); pd != "" {
		return pd
	}

	if fp := cfg.FilePath(); fp != "" {
		dir := filepath.Dir(fp)
		// If config is inside .weave/ directory, go up one level to project root.
		if filepath.Base(dir) == ".weave" {
			return filepath.Dir(dir)
		}

		return dir
	}

	dir, _ := os.Getwd()

	return dir
}

func (t *tool) Name() string { return "bash" }

//nolint:goconst // JSON schema keys are intentionally repeated literals.
func (t *tool) Definition() sdk.ToolDef {
	return sdk.ToolDef{
		Name:        "bash",
		Description: "Execute a bash command and return its output. Supports background execution. Provide exactly one of: command (to run), job_id (to check status), or kill_job (to terminate).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				ParamCommand: map[string]any{
					"type":        "string",
					"description": "The bash command to execute. Required when not using job_id or kill_job.",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds. Defaults to 120.",
				},
				"run_in_background": map[string]any{
					"type":        "boolean",
					"description": "Run the command in the background and return a job ID immediately.",
				},
				"auto_background_after": map[string]any{
					"type":        "number",
					"description": "Start command synchronously and move to background after N seconds if still running. 0 disables auto-background.",
				},
				"job_id": map[string]any{
					"type":        "string",
					"description": "Check the output and status of an existing background job by ID. When provided, returns the job's current output instead of running a new command.",
				},
				"kill_job": map[string]any{
					"type":        "string",
					"description": "Kill a background job by ID. When provided, terminates the specified job and returns its final status.",
				},
			},
			"additionalProperties": false,
		},
	}
}

func resolveTimeout(args map[string]any, base time.Duration) time.Duration {
	timeout := base
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	if v, ok := args["timeout"]; ok {
		if f, ok := v.(float64); ok && f > 0 {
			timeout = time.Duration(f) * time.Second
		}
	}

	return timeout
}

func resolveOperation(args map[string]any) (command, jobID, killJobID, errMsg string) {
	command, _ = args[ParamCommand].(string)
	jobID, _ = args["job_id"].(string)
	killJobID, _ = args["kill_job"].(string)

	opCount := 0
	if command != "" {
		opCount++
	}

	if jobID != "" {
		opCount++
	}

	if killJobID != "" {
		opCount++
	}

	if opCount == 0 {
		return "", "", "", "one of command, job_id, or kill_job is required"
	}

	if opCount > 1 {
		return "", "", "", "exactly one of command, job_id, or kill_job must be provided"
	}

	return command, jobID, killJobID, ""
}

func newRequestID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return prefix + "-" + hex.EncodeToString(b[:])
	}

	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), requestSeq.Add(1))
}

func guardianRequest(command, dir string) sdk.GuardianRequest {
	return sdk.GuardianRequest{
		ID:          newRequestID("bash-guardian"),
		ToolName:    "bash",
		Action:      sdk.GuardianActionExec,
		Command:     command,
		WorkingDir:  dir,
		Description: "Execute bash command",
		Metadata: map[string]any{
			"operation": "command",
		},
	}
}

func checkGuardian(ctx context.Context, command, dir string) (sdk.GuardianRequest, *sdk.ToolResult) {
	req := guardianRequest(command, dir)

	g := getGuardian()
	if g == nil {
		return req, nil
	}

	decision, err := g.Decide(ctx, req)
	if err != nil {
		return req, &sdk.ToolResult{Content: "guardian: " + err.Error(), IsError: true}
	}

	switch decision.Action {
	case sdk.GuardianDecisionAllow:
		return req, nil
	case sdk.GuardianDecisionBlock:
		return req, &sdk.ToolResult{Content: formatGuardianBlock(req, decision), IsError: true}
	default:
		decision.Action = sdk.GuardianDecisionBlock
		if decision.Reason == "" {
			decision.Reason = "guardian returned unresolved approval decision"
		}

		return req, &sdk.ToolResult{Content: formatGuardianBlock(req, decision), IsError: true}
	}
}

func formatGuardianBlock(req sdk.GuardianRequest, decision sdk.GuardianDecision) string {
	var b strings.Builder

	b.WriteString("guardian: blocked")
	b.WriteString("\naction: ")
	b.WriteString(string(req.Action))

	rule := decision.Profile
	if rule == "" {
		rule = decision.MatchedGrantID
	}
	if rule == "" {
		rule = decision.ID
	}
	if rule != "" {
		b.WriteString("\nrule: ")
		b.WriteString(rule)
	}

	if decision.Reason != "" {
		b.WriteString("\nreason: ")
		b.WriteString(decision.Reason)
	}

	return b.String()
}

func sandboxPlan(ctx context.Context, s sdk.Sandboxer, command, dir, guardianRequestID string, expansion *sdk.SandboxExpansion) (executionPlan, *sdk.ToolResult) {
	plan := executionPlan{command: command, dir: dir}
	if s == nil {
		return plan, nil
	}

	metadata := map[string]any{
		"operation":           "command",
		"guardian_request_id": guardianRequestID,
	}
	if expansion != nil {
		metadata["sandbox_expansion_id"] = expansion.ID
		metadata["sandbox_expansion_request_id"] = expansion.RequestID
		metadata["sandbox_expansion_retry"] = true
	}

	wrapped, err := s.WrapCommand(ctx, sdk.SandboxCommandRequest{
		ID:         newRequestID("bash-sandbox"),
		Command:    command,
		WorkingDir: dir,
		Metadata:   metadata,
	})
	if err != nil {
		if req, ok := expansionRequestFromError(err); ok {
			plan.expansionReq = &req

			return plan, nil
		}

		return plan, &sdk.ToolResult{Content: "sandbox: " + err.Error(), IsError: true}
	}

	plan.command = wrapped.Command
	plan.direct = true
	if len(wrapped.Args) > 0 {
		plan.args = append([]string(nil), wrapped.Args...)
	}
	if len(wrapped.Env) > 0 {
		plan.env = append([]string(nil), wrapped.Env...)
	}
	if wrapped.WorkingDir != "" {
		plan.dir = wrapped.WorkingDir
	}
	if req, ok := expansionRequestFromMetadata(wrapped.Metadata); ok {
		plan.expansionReq = &req
	}

	return plan, nil
}

func expansionRequestFromError(err error) (sdk.SandboxExpansionRequest, bool) {
	var sandboxErr sandboxExpansionRequestError
	if errors.As(err, &sandboxErr) {
		return sandboxErr.SandboxExpansionRequest(), true
	}

	return sdk.SandboxExpansionRequest{}, false
}

func expansionRequestFromMetadata(metadata map[string]any) (sdk.SandboxExpansionRequest, bool) {
	if req, ok := metadata["sandbox_expansion_request"].(sdk.SandboxExpansionRequest); ok {
		return req, true
	}

	return sdk.SandboxExpansionRequest{}, false
}

func requestSandboxExpansion(ctx context.Context, s sdk.Sandboxer, req sdk.SandboxExpansionRequest) (*sdk.SandboxExpansion, *sdk.ToolResult) {
	if req.ID == "" {
		req.ID = newRequestID("bash-sandbox-expansion")
	}

	expansion, err := s.RequestExpansion(ctx, req)
	if err != nil {
		return nil, &sdk.ToolResult{Content: "sandbox expansion: " + err.Error(), IsError: true}
	}

	switch expansion.State {
	case sdk.SandboxExpansionAllowed:
		return &expansion, nil
	case sdk.SandboxExpansionPending:
		return nil, &sdk.ToolResult{Content: "sandbox expansion pending: expansion was not approved before command execution", IsError: true}
	default:
		reason := expansion.Reason
		if reason == "" && expansion.Resolution != nil {
			reason = expansion.Resolution.Reason
		}
		if reason == "" {
			reason = "expansion was not approved"
		}

		return nil, &sdk.ToolResult{Content: "sandbox expansion denied: " + reason, IsError: true}
	}
}

func (t *tool) Execute(ctx context.Context, args map[string]any) (sdk.ToolResult, error) {
	command, jobID, killJobID, errMsg := resolveOperation(args)
	if errMsg != "" {
		return sdk.ToolResult{Content: "error: " + errMsg, IsError: true}, nil
	}

	if killJobID != "" {
		return t.killJob(killJobID), nil
	}

	if jobID != "" {
		return t.checkJob(jobID), nil
	}

	guardianReq, guardianResult := checkGuardian(ctx, command, t.dir)
	if guardianResult != nil {
		return *guardianResult, nil
	}

	originalCommand := command
	execDir := t.dir
	s := getSandboxer()

	plan, planResult := sandboxPlan(ctx, s, originalCommand, execDir, guardianReq.ID, nil)
	if planResult != nil {
		return *planResult, nil
	}
	if plan.expansionReq != nil {
		expansion, expansionResult := requestSandboxExpansion(ctx, s, *plan.expansionReq)
		if expansionResult != nil {
			return *expansionResult, nil
		}

		plan, planResult = sandboxPlan(ctx, s, originalCommand, execDir, guardianReq.ID, expansion)
		if planResult != nil {
			return *planResult, nil
		}
		if plan.expansionReq != nil {
			return sdk.ToolResult{Content: "sandbox expansion retry limit reached", IsError: true}, nil
		}
	}

	command = plan.command
	execArgs := plan.args
	execDir = plan.dir
	execEnv := plan.env
	execDirect := plan.direct

	timeout := resolveTimeout(args, t.timeout)
	runInBackground, _ := args["run_in_background"].(bool)

	autoBackgroundAfter := 0

	if v, ok := args["auto_background_after"]; ok {
		if f, ok := v.(float64); ok && f > 0 {
			autoBackgroundAfter = int(f)
		}
	}

	bus := sdk.BusFromContext(ctx)

	if runInBackground {
		if t.bgMgr == nil {
			return sdk.ToolResult{Content: "error: background manager not available", IsError: true}, nil
		}

		job := t.bgMgr.Start(command, execArgs, execDir, execEnv, execDirect, timeout, bus)

		return sdk.ToolResult{
			Content: fmt.Sprintf("Background job started: %s\nCommand: %s\nWait for completion or check output later.", job.ID, command),
		}, nil
	}

	if autoBackgroundAfter > 0 {
		if t.bgMgr == nil {
			return sdk.ToolResult{Content: "error: background manager not available", IsError: true}, nil
		}

		job := t.bgMgr.Start(command, execArgs, execDir, execEnv, execDirect, timeout, bus)

		timer := time.NewTimer(time.Duration(autoBackgroundAfter) * time.Second)
		defer timer.Stop()

		select {
		case <-job.done:
			return job.Result(), nil
		case <-timer.C:
			if job.IsDone() {
				return job.Result(), nil
			}

			output := job.Output()
			result := truncate.Truncate(output, truncate.DefaultMaxLines, truncate.DefaultMaxBytes)
			formatted := formatResultWithTempFile(result, output)
			content := fmt.Sprintf("%s\n\nBackground job %s is still running.\nCommand: %s", formatted, job.ID, command)

			return sdk.ToolResult{Content: content}, nil
		case <-ctx.Done():
			if killErr := t.bgMgr.Kill(job.ID); killErr != nil {
				return sdk.ToolResult{Content: "interrupted (failed to kill job: " + killErr.Error() + ")", IsError: true}, nil
			}

			job.Wait()

			return sdk.ToolResult{Content: "interrupted", IsError: true}, nil
		}
	}

	return t.executeSync(ctx, command, execArgs, execDir, execEnv, execDirect, timeout, bus)
}

func (t *tool) checkJob(jobID string) sdk.ToolResult {
	if t.bgMgr == nil {
		return sdk.ToolResult{Content: "error: background manager not available", IsError: true}
	}

	job, ok := t.bgMgr.Get(jobID)
	if !ok {
		return sdk.ToolResult{Content: fmt.Sprintf("error: job %s not found", jobID), IsError: true}
	}

	result := job.Result()

	status := "running"
	if job.IsDone() {
		status = "completed"
	}

	content := fmt.Sprintf("Job %s (%s)\nStatus: %s\n\n%s", jobID, job.Command, status, result.Content)

	return sdk.ToolResult{Content: content, IsError: result.IsError}
}

func (t *tool) killJob(jobID string) sdk.ToolResult {
	if t.bgMgr == nil {
		return sdk.ToolResult{Content: "error: background manager not available", IsError: true}
	}

	job, ok := t.bgMgr.Get(jobID)
	if !ok {
		return sdk.ToolResult{Content: fmt.Sprintf("error: job %s not found", jobID), IsError: true}
	}

	if job.IsDone() {
		result := job.Result()

		t.bgMgr.Remove(jobID)
		content := fmt.Sprintf("Job %s already completed.\n\n%s", jobID, result.Content)

		return sdk.ToolResult{Content: content, IsError: result.IsError}
	}

	if err := t.bgMgr.Kill(jobID); err != nil {
		return sdk.ToolResult{Content: fmt.Sprintf("error: failed to kill job %s: %s", jobID, err), IsError: true}
	}

	job.Wait()
	result := job.Result()

	t.bgMgr.Remove(jobID)

	content := fmt.Sprintf("Job %s killed.\n\n%s", jobID, result.Content)

	return sdk.ToolResult{Content: content, IsError: false}
}

func (t *tool) executeSync(ctx context.Context, command string, args []string, dir string, env []string, direct bool, timeout time.Duration, bus sdk.Bus) (sdk.ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := newExecCommand(ctx, command, args, env, direct)

	if dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			cmd.Dir = dir
		}
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}

		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}

			return fmt.Errorf("bash: kill process: %w", err)
		}

		return nil
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return sdk.ToolResult{Content: "error: " + err.Error(), IsError: true}, nil
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdoutPipe.Close()

		return sdk.ToolResult{Content: "error: " + err.Error(), IsError: true}, nil
	}

	if err := cmd.Start(); err != nil {
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()

		return sdk.ToolResult{Content: "error: " + err.Error(), IsError: true}, nil
	}

	var outBuf strings.Builder

	sw := &syncWriter{buf: &outBuf}

	var publishProgress func()

	if bus != nil {
		throttleCtx, cancelThrottle := context.WithCancel(ctx)
		defer cancelThrottle()

		publishProgress = sdk.Throttle(throttleCtx, func() {
			bus.Publish(sdk.NewEvent(sdk.TopicToolProgress, sdk.ToolProgress{
				ToolName: "bash",
				Content:  sw.String(),
			}))
		}, 200*time.Millisecond)
	}

	var wg sync.WaitGroup

	wg.Add(2)
	go collectStream(stdoutPipe, "stdout", bus, command, sw, publishProgress, &wg)
	go collectStream(stderrPipe, "stderr", bus, command, sw, publishProgress, &wg)

	waitErr := cmd.Wait()
	wg.Wait()

	// Publish final progress event with complete output.
	if bus != nil {
		bus.Publish(sdk.NewEvent(sdk.TopicToolProgress, sdk.ToolProgress{
			ToolName: "bash",
			Content:  sw.String(),
		}))
	}

	fullOutput := sw.String()

	if waitErr == nil {
		result := truncate.Truncate(fullOutput, truncate.DefaultMaxLines, truncate.DefaultMaxBytes)
		return sdk.ToolResult{Content: formatResultWithTempFile(result, fullOutput)}, nil
	}

	content, isErr := formatCmdError(fullOutput, waitErr, ctx)

	return sdk.ToolResult{Content: content, IsError: isErr}, nil
}

func newExecCommand(ctx context.Context, command string, args []string, env []string, direct bool) *exec.Cmd {
	if direct {
		cmd := exec.CommandContext(ctx, command, args...)
		if len(env) > 0 {
			cmd.Env = append([]string(nil), env...)
		}

		return cmd
	}

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	if len(env) > 0 {
		cmd.Env = append([]string(nil), env...)
	}

	return cmd
}

// syncWriter wraps a strings.Builder with a mutex for safe concurrent writes.
type syncWriter struct {
	buf *strings.Builder
	sync.Mutex
}

//nolint:wrapcheck // strings.Builder.Write never returns an error; wrapping is unnecessary
func (w *syncWriter) Write(p []byte) (int, error) {
	w.Lock()
	defer w.Unlock()

	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.Lock()
	defer w.Unlock()

	return w.buf.String()
}

// collectStream reads from r, writes raw bytes to outBuf, and publishes line
// events to bus when a complete line is read.
func collectStream(
	r io.Reader,
	stream string,
	bus sdk.Bus,
	command string,
	outBuf io.Writer,
	publishProgress func(),
	wg *sync.WaitGroup,
) {
	defer wg.Done()

	var lineBuf bytes.Buffer

	chunk := make([]byte, 4096)

	for {
		n, err := r.Read(chunk)
		if n > 0 {
			_, _ = outBuf.Write(chunk[:n])

			if publishProgress != nil {
				publishProgress()
			}

			lineBuf.Write(chunk[:n])

			for {
				data := lineBuf.Bytes()

				before, after, found := bytes.Cut(data, []byte{'\n'})
				if !found {
					break
				}

				if bus != nil {
					bus.Publish(sdk.NewEvent("tool.bash.output", BashOutputPayload{
						Command: command,
						Line:    strings.TrimSuffix(string(before), "\r"),
						Stream:  stream,
					}))
				}

				lineBuf.Reset()
				lineBuf.Write(after)
			}
		}

		if err != nil {
			break
		}
	}

	if lineBuf.Len() > 0 {
		if bus != nil {
			bus.Publish(sdk.NewEvent("tool.bash.output", BashOutputPayload{
				Command: command,
				Line:    strings.TrimSuffix(lineBuf.String(), "\r"),
				Stream:  stream,
			}))
		}
	}
}

func formatResultWithTempFile(result truncate.Result, fullOutput string) string {
	content := result.Format()
	if !result.Truncated {
		return content
	}

	tmpFile, err := os.CreateTemp("", "weave-bash-*.log")
	if err != nil {
		return content + "\n\nerror: could not save full output: " + err.Error()
	}

	_, writeErr := tmpFile.WriteString(fullOutput)

	closeErr := tmpFile.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmpFile.Name())

		var errMsg string
		if writeErr != nil {
			errMsg = writeErr.Error()
		} else {
			errMsg = closeErr.Error()
		}

		return content + "\n\nerror: could not save full output: " + errMsg
	}

	return content + "\n\nFull output saved to: " + tmpFile.Name()
}

func formatCmdError(fullOutput string, err error, ctx context.Context) (string, bool) {
	result := truncate.Truncate(fullOutput, truncate.DefaultMaxLines, truncate.DefaultMaxBytes)
	content := formatResultWithTempFile(result, fullOutput)

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() >= 0 {
		return fmt.Sprintf("%s\n[exit code %d]", content, exitErr.ExitCode()), false
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return content + "\nerror: command timed out", true
	case ctx.Err() == context.Canceled:
		return content + "\nerror: command canceled", true
	default:
		return fmt.Sprintf("%s\nerror: %s", content, err), true
	}
}
