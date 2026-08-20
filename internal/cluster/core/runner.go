package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// defaultStepTimeout bounds a single Ansible invocation when the caller did not
// supply one.
const defaultStepTimeout = 15 * time.Minute

// AnsibleSpec fully describes one ansible-playbook invocation. The engine
// supplies the engine-specific bits (playbook, inventory YAML, extra-vars, tags)
// and AnsibleRun owns the mechanical execution: temp workspace, file writes,
// argv assembly, process exec, output capture, and result mapping.
type AnsibleSpec struct {
	Bin             string         // ansible-playbook binary
	Playbook        string         // playbook path
	Inventory       string         // inventory YAML content
	ExtraVars       map[string]any // written to vars.json, passed as --extra-vars @file
	Tags            []string       // optional --tags values (joined with ",")
	Verbosity       int            // 0 = quiet; n>0 appends -vvv (n v's)
	StreamLogs      bool           // also tee stdout/stderr to the process streams
	MaxOutputChars  int            // cap on captured stdout/stderr (0 = unlimited)
	Timeout         time.Duration  // per-invocation timeout; <=0 uses defaultStepTimeout
	StepName        string         // recorded as StepResult.Name
	WorkspacePrefix string         // os.MkdirTemp prefix
	Env             []string       // extra environment appended to os.Environ()
}

/**
 * FailedStep builds a StepResult for a step that failed before Ansible was
 * even invoked (e.g. host-key pinning). It mirrors the shape AnsibleRun
 * produces on failure so callers can return it interchangeably.
 *
 * Params:
 *   name string - recorded as StepResult.Name
 *   err error - the reason the step failed
 *
 * Returns:
 *   StepResult - the resulting StepResult
 */
func FailedStep(name string, err error) StepResult {
	now := time.Now().UTC()
	return StepResult{
		Name:      name,
		Status:    JobStatusFailed,
		StartedAt: now,
		EndedAt:   now,
		ExitCode:  -1,
		Message:   err.Error(),
	}
}

/**
 * AnsibleRun executes one ansible-playbook invocation and maps its outcome to a
 * StepResult. Secrets travel via an on-disk vars.json (0600) passed with
 * `--extra-vars @file` — never on the command line or through a shell — so they
 * are not exposed in the process table and there is no shell-injection surface.
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   spec AnsibleSpec - the spec (AnsibleSpec)
 *
 * Returns:
 *   result StepResult - the result (StepResult)
 */
func AnsibleRun(ctx context.Context, spec AnsibleSpec) (result StepResult) {
	result = StepResult{
		Name:      spec.StepName,
		Status:    JobStatusRunning,
		StartedAt: time.Now().UTC(),
		ExitCode:  -1,
	}
	defer func() { result.EndedAt = time.Now().UTC() }()

	if strings.TrimSpace(spec.Playbook) == "" {
		result.Status = JobStatusFailed
		result.Message = "playbook path is not configured"
		return
	}

	workspace, err := os.MkdirTemp("", spec.WorkspacePrefix)
	if err != nil {
		result.Status = JobStatusFailed
		result.Message = fmt.Sprintf("create temp dir: %v", err)
		return
	}
	defer os.RemoveAll(workspace)

	inventoryPath := filepath.Join(workspace, "inventory.yml")
	varsPath := filepath.Join(workspace, "vars.json")

	if err := os.WriteFile(inventoryPath, []byte(spec.Inventory), 0o600); err != nil {
		result.Status = JobStatusFailed
		result.Message = fmt.Sprintf("write inventory: %v", err)
		return
	}

	sanitized, err := json.Marshal(spec.ExtraVars)
	if err != nil {
		result.Status = JobStatusFailed
		result.Message = fmt.Sprintf("marshal vars: %v", err)
		return
	}
	if err := os.WriteFile(varsPath, sanitized, 0o600); err != nil {
		result.Status = JobStatusFailed
		result.Message = fmt.Sprintf("write vars: %v", err)
		return
	}

	runTimeout := spec.Timeout
	if runTimeout <= 0 {
		runTimeout = defaultStepTimeout
	}
	stepCtx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()

	args := []string{"-i", inventoryPath, spec.Playbook}
	if len(spec.Tags) > 0 {
		args = append(args, "--tags", strings.Join(spec.Tags, ","))
	}
	args = append(args, "--extra-vars", "@"+varsPath)
	if spec.Verbosity > 0 {
		args = append(args, "-"+strings.Repeat("v", spec.Verbosity))
	}

	cmd := exec.CommandContext(stepCtx, spec.Bin, args...)
	// Pipelining collapses each module run from ~5 SSH round-trips (mkdir tmp,
	// sftp upload, chmod, execute, cleanup) into one, so a node whose sshd is
	// slow (e.g. under clone/recovery IO load) delays a task by seconds, not
	// minutes. Safe here: nodes are Debian/Ubuntu, whose sudoers never sets
	// requiretty. Listed before spec.Env so callers can still override it.
	cmd.Env = append(os.Environ(), "ANSIBLE_PIPELINING=True")
	cmd.Env = append(cmd.Env, spec.Env...)

	var stdout, stderr cappedBuffer
	stdout.limit = spec.MaxOutputChars
	stderr.limit = spec.MaxOutputChars
	if spec.StreamLogs {
		cmd.Stdout = io.MultiWriter(&stdout, os.Stdout)
		cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}

	err = cmd.Run()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	if err == nil {
		result.Status = JobStatusCompleted
		result.ExitCode = 0
		return
	}

	result.Status = JobStatusFailed
	if exitErr, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitErr.ExitCode()
	} else {
		result.ExitCode = 1
	}
	if stepCtx.Err() == context.DeadlineExceeded {
		result.Message = "step execution timed out"
		return
	}
	result.Message = fmt.Sprintf("ansible step failed: %v", err)
	if hint := ExplainAnsibleFailure(result.Stdout + result.Stderr); hint != "" {
		result.Message += " — " + hint
	}
	return
}

/**
 * ExplainAnsibleFailure turns a failure whose real cause is legible in the
 * output, but not in the exit code, into a sentence that names it.
 *
 * The case that motivates it: a node going down mid-run reports only
 * UNREACHABLE, which reads as a network or SSH-credential problem and gets
 * investigated as one. It is neither — sshd is refusing logins because the
 * machine is shutting down, and something outside erawan ordered that.
 * CloudStack-side VM stops (a scale or restart flow acting on a cluster while
 * one of its jobs is still running) bypass the member-op lock entirely, so
 * erawan cannot serialise against them and can only report them accurately.
 *
 * Params:
 *   output string - combined stdout and stderr of the ansible run
 *
 * Returns:
 *   string - a one-sentence explanation, or "" when nothing is recognised
 */
func ExplainAnsibleFailure(output string) string {
	// pam_nologin's wording differs slightly by distro, so match on the parts
	// that do not: the nologin module itself, and the sentence it prints.
	shutdownSignatures := []string{
		"System is going down",
		"pam_nologin",
		"Unprivileged users are not permitted to log in",
	}
	for _, sig := range shutdownSignatures {
		if strings.Contains(output, sig) {
			return "a target node was shutting down while this step ran, so SSH was refused (pam_nologin) — " +
				"an external stop, reboot or scale of the VM raced this job. " +
				"Erawan cannot see CloudStack-side VM operations and cannot lock against them: " +
				"wait for the VM operation to finish, then re-run this job."
		}
	}
	return ""
}

// cappedBuffer is a write-capped buffer that preserves both ends of the
// stream: the first half of the limit verbatim, plus a ring of the most
// recent output. Ansible prints failures and the play recap last, so keeping
// only the head (as a plain capped buffer would) hides exactly the part of a
// long -vvv log that explains why a step failed. limit=0 means unlimited.
type cappedBuffer struct {
	head    bytes.Buffer
	tail    []byte
	limit   int
	dropped bool
}

/**
 * Write.
 *
 * Receiver:
 *   b *cappedBuffer - pointer receiver; the method may mutate this cappedBuffer instance
 *
 * Params:
 *   p []byte - the p bytes
 *
 * Returns:
 *   int - the resulting integer
 *   error - error value; non-nil when the operation fails
 */
func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return b.head.Write(p)
	}
	n := len(p)
	if avail := b.limit/2 - b.head.Len(); avail > 0 {
		if len(p) <= avail {
			_, _ = b.head.Write(p)
			return n, nil
		}
		_, _ = b.head.Write(p[:avail])
		p = p[avail:]
	}
	tailMax := b.limit - b.limit/2
	switch {
	case len(p) >= tailMax:
		b.tail = append(b.tail[:0], p[len(p)-tailMax:]...)
		b.dropped = true
	case len(b.tail)+len(p) <= tailMax:
		b.tail = append(b.tail, p...)
	default:
		over := len(b.tail) + len(p) - tailMax
		b.tail = append(b.tail[:copy(b.tail, b.tail[over:])], p...)
		b.dropped = true
	}
	return n, nil
}

/**
 * String.
 *
 * Receiver:
 *   b *cappedBuffer - pointer receiver; the method may mutate this cappedBuffer instance
 *
 * Returns:
 *   string - the resulting string
 */
func (b *cappedBuffer) String() string {
	s := b.head.String()
	if len(b.tail) > 0 {
		if b.dropped {
			s += "\n...truncated; most recent output follows...\n"
		}
		s += string(b.tail)
	}
	return strings.TrimSpace(s)
}

/**
 * WaitForSSH blocks until every host accepts a TCP connection on port, or the
 * budget runs out. It is a liveness probe, not an authentication check: a node
 * that answers here may still be finishing its boot, which is what the
 * playbooks own wait_for_connection covers.
 *
 * Its purpose is the window a vertical scale opens. CloudStack stops the VM to
 * resize it, so a cluster restart issued in that window finds the node going
 * down (or already down) through no fault of the cluster. Waiting for the VM to
 * come back and running the step again is the correct response to that, and the
 * recovery steps are idempotent precisely so it can be.
 *
 * Params:
 *   ctx context.Context - context carrying cancellation signals and deadlines
 *   hosts []string - addresses to probe
 *   port int - TCP port to probe (the cluster SSH port)
 *   budget time.Duration - how long to keep trying before giving up
 *
 * Returns:
 *   error - nil once every host answers, otherwise the reason the wait ended
 */
func WaitForSSH(ctx context.Context, hosts []string, port int, budget time.Duration) error {
	if len(hosts) == 0 {
		return nil
	}
	if port <= 0 {
		port = 22
	}
	deadline := time.Now().Add(budget)
	for {
		pending := ""
		for _, h := range hosts {
			addr := net.JoinHostPort(h, fmt.Sprint(port))
			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				pending = h
				break
			}
			_ = conn.Close()
		}
		if pending == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not accept connections on port %d within %s", pending, port, budget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
