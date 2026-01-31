package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"novakey/cmd/novakey-runner/internal/registry"
)

// Runner executes allowlisted actions from a loaded registry.
// It enforces per-action cooldown and concurrency limits (defense-in-depth).
type Runner struct {
	reg *registry.Config

	mu        sync.Mutex
	inflight  map[string]int
	lastStart map[string]time.Time
}

func New(reg *registry.Config) *Runner {
	return &Runner{
		reg:       reg,
		inflight:  map[string]int{},
		lastStart: map[string]time.Time{},
	}
}

type Result struct {
	OK        bool
	Err       string
	ExitCode  int
	Duration  time.Duration
	StdoutB64 string
	StderrB64 string
}

func (r *Runner) Execute(ctx context.Context, actionID string, params map[string]any) (Result, error) {
	a, ok := r.reg.Actions[actionID]
	if !ok {
		return Result{OK: false, Err: "unknown action"}, nil
	}
	if len(a.Exec) == 0 {
		return Result{OK: false, Err: "invalid action exec"}, nil
	}

	// Validate params (strong typing + regex + unknown param rejection)
	vals, err := a.ValidateParams(params)
	if err != nil {
		return Result{OK: false, Err: "param validation failed: " + err.Error()}, nil
	}

	// Cooldown/concurrency
	if err := r.begin(actionID, a); err != nil {
		return Result{OK: false, Err: err.Error()}, nil
	}
	defer r.end(actionID)

	// Build argv with per-arg template substitution
	argv := make([]string, 0, len(a.Exec))
	for _, arg := range a.Exec {
		s, err := registry.SubstituteArg(arg, vals)
		if err != nil {
			return Result{OK: false, Err: "template substitution failed: " + err.Error()}, nil
		}
		argv = append(argv, s)
	}

	timeout := a.EffectiveTimeout()
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	cmd := exec.CommandContext(ctx2, argv[0], argv[1:]...)
	cmd.Dir = a.Policy.WorkDir
	cmd.Env = cleanEnv(a.Policy.Env)

	stdout, stderr, runErr := runWithCaps(cmd, a.Policy.MaxStdoutBytes, a.Policy.MaxStderrBytes)

	dur := time.Since(start)
	res := Result{
		OK:        runErr == nil,
		ExitCode:  exitCode(runErr),
		Duration:  dur,
		StdoutB64: base64.StdEncoding.EncodeToString(stdout),
		StderrB64: base64.StdEncoding.EncodeToString(stderr),
	}
	if runErr != nil {
		// Keep this short; daemon can log more context.
		res.Err = classifyErr(runErr)
	}
	return res, nil
}

func (r *Runner) begin(actionID string, a registry.Action) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Cooldown
	if a.Policy.CooldownMS > 0 {
		last := r.lastStart[actionID]
		if !last.IsZero() && time.Since(last) < time.Duration(a.Policy.CooldownMS)*time.Millisecond {
			return fmt.Errorf("action cooldown active")
		}
	}

	// Concurrency
	maxC := a.Policy.MaxConcurrency
	if maxC <= 0 {
		maxC = 1
	}
	if r.inflight[actionID] >= maxC {
		return fmt.Errorf("action concurrency limit reached")
	}

	r.inflight[actionID]++
	r.lastStart[actionID] = time.Now()
	return nil
}

func (r *Runner) end(actionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[actionID] > 0 {
		r.inflight[actionID]--
	}
}

func runWithCaps(cmd *exec.Cmd, maxOut, maxErr int) ([]byte, []byte, error) {
	if maxOut <= 0 {
		maxOut = 65536
	}
	if maxErr <= 0 {
		maxErr = 65536
	}
	var outBuf, errBuf cappedBuffer
	outBuf.cap = maxOut
	errBuf.cap = maxErr

	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	return outBuf.Bytes(), errBuf.Bytes(), err
}

type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	cap int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remain := c.cap - c.buf.Len()
	if remain <= 0 {
		// Discard but report written to avoid blocking writers
		return len(p), nil
	}
	if len(p) > remain {
		_, _ = c.buf.Write(p[:remain])
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func cleanEnv(env map[string]string) []string {
	// Minimal env: only what registry specifies.
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func deadlineOf(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now()
}

func classifyErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "exec_failed"
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	// If Run fails because the process exits non-zero, the error is *ExitError
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	// If we couldn't start the process, treat as -1
	return -1
}
