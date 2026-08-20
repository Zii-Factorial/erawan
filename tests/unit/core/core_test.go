// Package core_test holds black-box unit tests for internal/cluster/core,
// exercising only its exported API.
package core_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"erawan-cluster/internal/cluster/core"
)

// spec and secret are minimal stand-ins for an engine's stored spec/secret.
type spec struct {
	Name string `json:"name"`
}

type secret struct {
	Pass string `json:"pass"`
}

// Valid 24-char hex job IDs for use in tests.
const (
	id1 = "000000000000000000000001"
	id2 = "000000000000000000000002"
	id3 = "000000000000000000000003"
	id4 = "000000000000000000000004"
	id5 = "000000000000000000000005"
	id6 = "000000000000000000000006"
	id7 = "000000000000000000000007"
)

func newStore(t *testing.T) *core.Store[spec, secret] {
	t.Helper()
	s, err := core.NewStore[spec, secret](t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestStoreSaveLoadAndSecretRoundTrip(t *testing.T) {
	s := newStore(t)
	job := &core.Job[spec]{ID: id1, Status: core.JobStatusRunning, Request: spec{Name: "cluster-a"}}
	if err := s.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}
	if job.UpdatedAt.IsZero() {
		t.Fatal("Save should stamp UpdatedAt")
	}
	if err := s.SaveSecret(id1, secret{Pass: "s3cr3t"}); err != nil {
		t.Fatalf("save secret: %v", err)
	}

	loaded, err := s.Load(id1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Request.Name != "cluster-a" {
		t.Fatalf("round-trip mismatch: %+v", loaded.Request)
	}
	sec, err := s.LoadSecret(id1)
	if err != nil {
		t.Fatalf("load secret: %v", err)
	}
	if sec.Pass != "s3cr3t" {
		t.Fatalf("secret round-trip mismatch: %q", sec.Pass)
	}
}

func TestStoreLoadMissingReturnsError(t *testing.T) {
	s := newStore(t)
	if _, err := s.Load(id2); err == nil {
		t.Fatal("expected error loading missing job")
	}
	if _, err := s.LoadSecret(id2); err == nil {
		t.Fatal("expected error loading missing secret")
	}
}

func TestStoreListNewestFirstAndLimit(t *testing.T) {
	s := newStore(t)
	for _, jobID := range []string{id3, id4, id5} {
		if err := s.Save(&core.Job[spec]{ID: jobID, Status: core.JobStatusCompleted}); err != nil {
			t.Fatalf("save %s: %v", jobID, err)
		}
		time.Sleep(10 * time.Millisecond) // distinct mod times for ordering
	}
	jobs, err := s.List(2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected limit=2 jobs, got %d", len(jobs))
	}
	if jobs[0].ID != id5 {
		t.Fatalf("expected newest job %q first, got %q", id5, jobs[0].ID)
	}
}

func TestStoreListExcludesSecretSidecars(t *testing.T) {
	s := newStore(t)
	if err := s.Save(&core.Job[spec]{ID: id1, Status: core.JobStatusCompleted}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SaveSecret(id1, secret{Pass: "p"}); err != nil {
		t.Fatalf("save secret: %v", err)
	}
	jobs, err := s.List(0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected secret sidecar excluded, got %d jobs", len(jobs))
	}
}

func TestStoreMoveJobsToCopiesAndRemovesSourceFiles(t *testing.T) {
	sourceDir := t.TempDir()
	source, err := core.NewStore[spec, secret](sourceDir)
	if err != nil {
		t.Fatalf("new source store: %v", err)
	}
	dest, err := core.NewStore[spec, secret](t.TempDir())
	if err != nil {
		t.Fatalf("new dest store: %v", err)
	}
	if err := source.Save(&core.Job[spec]{ID: id2, Status: core.JobStatusCompleted, Request: spec{Name: "moved"}}); err != nil {
		t.Fatalf("save source job: %v", err)
	}
	if err := source.SaveSecret(id2, secret{Pass: "pw"}); err != nil {
		t.Fatalf("save source secret: %v", err)
	}

	if err := source.MoveJobsTo(dest); err != nil {
		t.Fatalf("move jobs: %v", err)
	}
	loaded, err := dest.Load(id2)
	if err != nil {
		t.Fatalf("load moved job: %v", err)
	}
	if loaded.Request.Name != "moved" {
		t.Fatalf("unexpected moved job: %+v", loaded.Request)
	}
	sec, err := dest.LoadSecret(id2)
	if err != nil {
		t.Fatalf("load moved secret: %v", err)
	}
	if sec.Pass != "pw" {
		t.Fatalf("unexpected moved secret: %+v", sec)
	}
	if _, err := os.Stat(filepath.Join(sourceDir, id2+".json")); !os.IsNotExist(err) {
		t.Fatalf("expected source job file removed, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(sourceDir, id2+".secret.json")); !os.IsNotExist(err) {
		t.Fatalf("expected source secret file removed, got %v", err)
	}
}

func TestStoreMarkStaleRunningJobsFailed(t *testing.T) {
	s := newStore(t)
	_ = s.Save(&core.Job[spec]{ID: id6, Status: core.JobStatusRunning})
	_ = s.Save(&core.Job[spec]{ID: id7, Status: core.JobStatusCompleted})

	s.MarkStaleRunningJobsFailed()

	r, _ := s.Load(id6)
	if r.Status != core.JobStatusFailed {
		t.Fatalf("expected stale running job marked failed, got %q", r.Status)
	}
	if r.Error == "" {
		t.Fatal("expected an error message on the failed job")
	}
	d, _ := s.Load(id7)
	if d.Status != core.JobStatusCompleted {
		t.Fatalf("completed job must be untouched, got %q", d.Status)
	}
}

func TestStoreUpdateIsAtomicReadModifyWrite(t *testing.T) {
	s := newStore(t)
	_ = s.Save(&core.Job[spec]{ID: id1, Status: core.JobStatusRunning, Request: spec{Name: "n0"}})

	if err := s.Update(id1, func(j *core.Job[spec]) error {
		j.Request.Name = "n1"
		return nil
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	loaded, _ := s.Load(id1)
	if loaded.Request.Name != "n1" {
		t.Fatalf("expected update persisted, got %q", loaded.Request.Name)
	}
}

func TestApplyProgressCountsCompletedSteps(t *testing.T) {
	job := &core.Job[spec]{
		Status: core.JobStatusRunning,
		Steps: []core.StepResult{
			{Status: core.JobStatusCompleted},
			{Status: core.JobStatusSkipped},
			{Status: core.JobStatusCompleted},
		},
	}
	core.ApplyProgress(job, 4)
	if job.TotalSteps != 4 || job.CompletedSteps != 2 || job.ProgressPercent != 50 {
		t.Fatalf("unexpected progress: total=%d completed=%d pct=%d", job.TotalSteps, job.CompletedSteps, job.ProgressPercent)
	}
}

func TestApplyProgressCompletedJobReportsFull(t *testing.T) {
	job := &core.Job[spec]{Status: core.JobStatusCompleted}
	core.ApplyProgress(job, 7)
	if job.CompletedSteps != 7 || job.ProgressPercent != 100 {
		t.Fatalf("completed job should report 100%%, got completed=%d pct=%d", job.CompletedSteps, job.ProgressPercent)
	}
}

func TestApplyProgressZeroStepsIsZeroPercent(t *testing.T) {
	job := &core.Job[spec]{Status: core.JobStatusRunning}
	core.ApplyProgress(job, 0)
	if job.ProgressPercent != 0 {
		t.Fatalf("expected 0%% for zero steps, got %d", job.ProgressPercent)
	}
}

func TestNewJobIDAndRandomSecret(t *testing.T) {
	if a, b := core.NewJobID(), core.NewJobID(); a == b || len(a) != 24 {
		t.Fatalf("job ids should be unique 24-hex strings, got %q %q", a, b)
	}
	if core.OrRandomSecret("given") != "given" {
		t.Fatal("OrRandomSecret should return a provided value unchanged")
	}
	if g := core.OrRandomSecret(""); len(g) != 48 {
		t.Fatalf("OrRandomSecret(\"\") should be 48 hex chars, got %d", len(g))
	}
}

func TestSSHPolicySecureByDefault(t *testing.T) {
	p := core.SSHPolicy{VerifyHostKeys: true, KnownHostsFile: "/etc/erawan/known_hosts"}
	if env := p.AnsibleEnv(); len(env) != 2 || env[0] != "ANSIBLE_HOST_KEY_CHECKING=True" || env[1] != "ANSIBLE_TIMEOUT=30" {
		t.Fatalf("expected host key checking enabled and connection timeout set, got %v", env)
	}
	args := p.SSHCommonArgs()
	if !strings.Contains(args, "StrictHostKeyChecking=yes") || !strings.Contains(args, "UserKnownHostsFile=/etc/erawan/known_hosts") {
		t.Fatalf("unexpected secure ssh args: %q", args)
	}
}

func TestSSHPolicyInsecureOptOut(t *testing.T) {
	p := core.SSHPolicy{VerifyHostKeys: false}
	if env := p.AnsibleEnv(); env[0] != "ANSIBLE_HOST_KEY_CHECKING=False" {
		t.Fatalf("expected host key checking disabled, got %v", env)
	}
	if args := p.SSHCommonArgs(); !strings.Contains(args, "StrictHostKeyChecking=no") {
		t.Fatalf("expected StrictHostKeyChecking=no, got %q", args)
	}
}

func TestLauncherCapsConcurrency(t *testing.T) {
	const limit = 2
	l := core.NewLauncher(limit)
	var current, peak int64
	var mu sync.Mutex
	release := make(chan struct{})

	for i := 0; i < 6; i++ {
		l.Go(func() {
			n := atomic.AddInt64(&current, 1)
			mu.Lock()
			if n > peak {
				peak = n
			}
			mu.Unlock()
			<-release
			atomic.AddInt64(&current, -1)
		})
	}
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&current); got > limit {
		t.Fatalf("expected at most %d concurrent jobs, got %d", limit, got)
	}
	close(release)
	l.Wait(context.Background())
	if peak == 0 || peak > limit {
		t.Fatalf("peak concurrency %d outside (0, %d]", peak, limit)
	}
}

func TestLauncherWaitRespectsContext(t *testing.T) {
	l := core.NewLauncher(1)
	block := make(chan struct{})
	l.Go(func() { <-block })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	l.Wait(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("Wait did not return when context expired")
	}
	close(block)
}

func TestAnsibleRunRejectsUnconfiguredPlaybook(t *testing.T) {
	res := core.AnsibleRun(context.Background(), core.AnsibleSpec{Bin: "true", StepName: "deploy"})
	if res.Status != core.JobStatusFailed || !strings.Contains(res.Message, "playbook path is not configured") {
		t.Fatalf("expected not-configured failure, got %+v", res)
	}
}

func TestStoreRejectsPathTraversalJobIDs(t *testing.T) {
	s := newStore(t)
	bad := []string{
		"../etc/passwd",
		"../../secret",
		"../pgsql/" + id1,
		"short",
		"",
		"AABBCCDDEE1122334455AABB", // uppercase not allowed
		id1 + "extra",              // too long
	}
	for _, jobID := range bad {
		if _, err := s.Load(jobID); err == nil {
			t.Errorf("Load(%q): expected error, got nil", jobID)
		}
		if _, err := s.LoadSecret(jobID); err == nil {
			t.Errorf("LoadSecret(%q): expected error, got nil", jobID)
		}
		if err := s.Update(jobID, func(j *core.Job[spec]) error { return nil }); err == nil {
			t.Errorf("Update(%q): expected error, got nil", jobID)
		}
		if err := s.SaveSecret(jobID, secret{}); err == nil {
			t.Errorf("SaveSecret(%q): expected error, got nil", jobID)
		}
	}
}

func TestAnsibleRunSuccessAndFailureExitCodes(t *testing.T) {
	ok := core.AnsibleRun(context.Background(), core.AnsibleSpec{
		Bin: "true", Playbook: "playbook.yml", StepName: "deploy", WorkspacePrefix: "core-test-",
	})
	if ok.Status != core.JobStatusCompleted || ok.ExitCode != 0 {
		t.Fatalf("expected success with /usr/bin/true, got %+v", ok)
	}
	fail := core.AnsibleRun(context.Background(), core.AnsibleSpec{
		Bin: "false", Playbook: "playbook.yml", StepName: "deploy", WorkspacePrefix: "core-test-",
	})
	if fail.Status != core.JobStatusFailed || fail.ExitCode == 0 {
		t.Fatalf("expected failure with /usr/bin/false, got %+v", fail)
	}
}

func TestEnsureKnownHostsNoopWhenVerificationOff(t *testing.T) {
	p := core.SSHPolicy{VerifyHostKeys: false, KnownHostsFile: filepath.Join(t.TempDir(), "known_hosts")}
	if err := p.EnsureKnownHosts(context.Background(), []string{"203.0.113.1"}, 22, false); err != nil {
		t.Fatalf("expected no-op with VerifyHostKeys=false, got %v", err)
	}
	if _, err := os.Stat(p.KnownHostsFile); !os.IsNotExist(err) {
		t.Fatalf("expected known_hosts file not to be created, stat err: %v", err)
	}
}

func TestEnsureKnownHostsNoopWhenNoKnownHostsFile(t *testing.T) {
	p := core.SSHPolicy{VerifyHostKeys: true}
	if err := p.EnsureKnownHosts(context.Background(), []string{"203.0.113.1"}, 22, false); err != nil {
		t.Fatalf("expected no-op without a known_hosts file, got %v", err)
	}
}

func TestEnsureKnownHostsSkipsAlreadyPinnedHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	const line = "10.10.0.99 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI-fake-key-for-test\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}
	p := core.SSHPolicy{VerifyHostKeys: true, KnownHostsFile: path}
	// The host is already pinned, so this must not shell out to ssh-keyscan
	// (which would otherwise block/fail against a fake IP).
	if err := p.EnsureKnownHosts(context.Background(), []string{"10.10.0.99"}, 22, false); err != nil {
		t.Fatalf("expected no scan needed for already-pinned host, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != line {
		t.Fatalf("expected known_hosts to be unchanged, got %q (err %v)", got, err)
	}
}

func TestEnsureKnownHostsSkipsAlreadyPinnedHostOnNonStandardPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	// ssh-keyscan/ssh bracket the host for non-default ports: "[host]:port".
	const line = "[10.10.0.99]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI-fake-key-for-test\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}
	p := core.SSHPolicy{VerifyHostKeys: true, KnownHostsFile: path}
	if err := p.EnsureKnownHosts(context.Background(), []string{"10.10.0.99"}, 2222, false); err != nil {
		t.Fatalf("expected no scan needed for already-pinned bracketed host, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != line {
		t.Fatalf("expected known_hosts to be unchanged, got %q (err %v)", got, err)
	}
}

func TestEnsureKnownHostsResetForgetsPinnedHostAndForcesRescan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	const line = "127.0.0.1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI-stale-fake-key\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}
	p := core.SSHPolicy{VerifyHostKeys: true, KnownHostsFile: path}

	// Without reset, the pinned host is skipped and no scan is attempted.
	if err := p.EnsureKnownHosts(context.Background(), []string{"127.0.0.1"}, 1, false); err != nil {
		t.Fatalf("expected no scan needed for already-pinned host, got %v", err)
	}

	// With reset, the stale entry is forgotten and a rescan is attempted —
	// which fails fast against the refusing port, proving the scan ran.
	if err := p.EnsureKnownHosts(context.Background(), []string{"127.0.0.1"}, 1, true); err == nil {
		t.Fatal("expected reset to force a rescan that fails against the refusing port")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if strings.Contains(string(got), "stale-fake-key") {
		t.Fatalf("expected stale entry to be removed by reset, got %q", got)
	}
}

func TestEnsureKnownHostsFailsFastForUnreachableHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	p := core.SSHPolicy{VerifyHostKeys: true, KnownHostsFile: path}
	start := time.Now()
	// Port 1 on loopback refuses connections immediately, so ssh-keyscan
	// should fail fast rather than block for the full keyscan timeout.
	err := p.EnsureKnownHosts(context.Background(), []string{"127.0.0.1"}, 1, false)
	if err == nil {
		t.Fatal("expected an error for an unreachable host")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("expected fast failure on connection refused, took %v", elapsed)
	}
	if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("expected error to name the host, got %q", err)
	}
}

// A node going down mid-run surfaces only as UNREACHABLE, which reads as a
// network or credential problem. These are the lines that actually explain it,
// taken from a real recover that a CloudStack-side VM stop raced.
func TestExplainAnsibleFailureNamesAMidRunShutdown(t *testing.T) {
	realOutput := `<10.10.0.238> (255, b'', b'mux_client_request_session: read from master failed: Broken pipe\r\n` +
		`System is going down. Unprivileged users are not permitted to log in anymore. ` +
		`For technical details, see pam_nologin(8).\n\nConnection closed by 10.10.0.238 port 22\r\n')
fatal: [standby_1]: UNREACHABLE!`

	got := core.ExplainAnsibleFailure(realOutput)
	if got == "" {
		t.Fatal("expected a mid-run shutdown to be explained, got no explanation")
	}
	for _, want := range []string{"shutting down", "raced this job"} {
		if !strings.Contains(got, want) {
			t.Fatalf("explanation %q does not mention %q", got, want)
		}
	}
}

func TestExplainAnsibleFailureStaysQuietOnOrdinaryFailures(t *testing.T) {
	cases := map[string]string{
		"task failure":     "fatal: [primary]: FAILED! => {\"msg\": \"Patroni did not reach a healthy state\"}",
		"ssh auth failure": "fatal: [primary]: UNREACHABLE! => {\"msg\": \"Permission denied (publickey)\"}",
		"empty":            "",
	}
	for name, out := range cases {
		if got := core.ExplainAnsibleFailure(out); got != "" {
			t.Fatalf("%s: expected no explanation, got %q", name, got)
		}
	}
}

func TestWaitForSSHReturnsOnceHostsAnswer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	if err := core.WaitForSSH(context.Background(), []string{"127.0.0.1"}, port, 5*time.Second); err != nil {
		t.Fatalf("expected a listening host to satisfy the wait, got %v", err)
	}
}

func TestWaitForSSHGivesUpOnADeadHost(t *testing.T) {
	// Port 1 on loopback: nothing listens, so this is refused immediately and
	// the wait must end on its own budget rather than blocking the job.
	start := time.Now()
	err := core.WaitForSSH(context.Background(), []string{"127.0.0.1"}, 1, 1*time.Second)
	if err == nil {
		t.Fatal("expected a dead host to end the wait with an error")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("wait overran its budget: %s", elapsed)
	}
}

func TestWaitForSSHIsANoOpWithoutHosts(t *testing.T) {
	if err := core.WaitForSSH(context.Background(), nil, 22, time.Second); err != nil {
		t.Fatalf("expected no hosts to be a no-op, got %v", err)
	}
}
