// Package pgsql_test holds black-box unit tests for the PostgreSQL cluster
// service and its database manager, exercising only their exported API.
package pgsql_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"erawan-cluster/internal/cluster/core"
	pgsql "erawan-cluster/internal/cluster/pgsql"
	dbmanager "erawan-cluster/internal/cluster/pgsql/dbmanager"
)

func tempKey(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(p, []byte("test-key"), 0o600); err != nil {
		t.Fatalf("write temp key: %v", err)
	}
	return p
}

func newService(t *testing.T) (*pgsql.Service, pgsql.Store) {
	t.Helper()
	store, err := pgsql.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	svc := pgsql.NewService(store, nil) // nil runner: no real ansible is executed
	if err := svc.SetSSHConfig("clusterops", tempKey(t)); err != nil {
		t.Fatalf("set ssh config: %v", err)
	}
	return svc, store
}

func TestValidateDeployRequestAppliesDefaults(t *testing.T) {
	req := pgsql.DeployRequest{PrimaryIP: "10.0.0.1"}
	if err := pgsql.ValidateDeployRequest(&req); err != nil {
		t.Fatalf("expected valid request, got %v", err)
	}
	if req.SSHPort != 22 || req.PostgresPort != 5432 || req.PostgresVersion != 16 {
		t.Fatalf("unexpected defaults: ssh=%d pg=%d ver=%d", req.SSHPort, req.PostgresPort, req.PostgresVersion)
	}
	if req.AdminUsername != "admin" || req.ClusterName != "postgres-cluster" {
		t.Fatalf("unexpected name defaults: admin=%q cluster=%q", req.AdminUsername, req.ClusterName)
	}
	if req.ConnectionLimit != 0 {
		t.Fatalf("expected connection_limit to default to 0 (engine default), got %d", req.ConnectionLimit)
	}

	withLimit := pgsql.DeployRequest{PrimaryIP: "10.0.0.1", ConnectionLimit: 500}
	if err := pgsql.ValidateDeployRequest(&withLimit); err != nil {
		t.Fatalf("expected connection_limit 500 to be valid, got %v", err)
	}
}

func TestValidateDeployRequestRejectsBadInput(t *testing.T) {
	cases := map[string]pgsql.DeployRequest{
		"bad primary ip":            {PrimaryIP: "not-an-ip"},
		"bad standby ip":            {PrimaryIP: "10.0.0.1", StandbyIPs: []string{"x"}},
		"unsupported pg version":    {PrimaryIP: "10.0.0.1", PostgresVersion: 99},
		"connection limit too low":  {PrimaryIP: "10.0.0.1", ConnectionLimit: 5},
		"connection limit below patroni minimum": {PrimaryIP: "10.0.0.1", ConnectionLimit: 20},
		"connection limit too high": {PrimaryIP: "10.0.0.1", ConnectionLimit: 100001},
	}
	for name, req := range cases {
		req := req
		t.Run(name, func(t *testing.T) {
			if err := pgsql.ValidateDeployRequest(&req); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

const testJobID = "aabbccddee1122334455aabb"

func TestValidateMemberRequests(t *testing.T) {
	if err := pgsql.ValidateAddMemberRequest(&pgsql.AddMemberRequest{}); err == nil {
		t.Fatal("expected error for empty add-member request")
	}
	add := &pgsql.AddMemberRequest{JobID: testJobID, MemberIPs: []string{"10.0.0.5"}}
	if err := pgsql.ValidateAddMemberRequest(add); err != nil {
		t.Fatalf("expected valid add-member request, got %v", err)
	}
	if err := pgsql.ValidateRemoveMemberRequest(&pgsql.RemoveMemberRequest{JobID: testJobID, MemberIP: "bad"}); err == nil {
		t.Fatal("expected error for invalid remove-member IP")
	}
}

func TestSetSSHConfigRejectsInvalidUser(t *testing.T) {
	svc := pgsql.NewService(nil, nil)
	if err := svc.SetSSHConfig("bad user!", tempKey(t)); err == nil {
		t.Fatal("expected invalid ssh user to be rejected")
	}
}

// Two add/remove-member calls racing against the same cluster both mutate
// etcd/Patroni membership, which can transiently break quorum on the
// primary. AddMember must reject a second call while one is already in
// flight rather than let them race inside Ansible.
func TestAddMemberRejectsWhileAnotherMemberOpRunning(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&pgsql.Job{
		ID:                testJobID,
		Status:            pgsql.JobStatusCompleted,
		Request:           pgsql.StoredSpec{ClusterName: "prod", PrimaryIP: "10.0.0.1"},
		ActiveMemberJobID: "already-running-job",
	})
	_ = store.SaveSecret(testJobID, pgsql.StoredSecret{PostgresUser: "postgres", PostgresPassword: "pw", AdminPassword: "pw"})

	if _, err := svc.AddMember(context.Background(), pgsql.AddMemberRequest{JobID: testJobID, MemberIPs: []string{"10.0.0.5"}}); err == nil {
		t.Fatal("expected AddMember to reject when another member operation is already running")
	}
}

func TestAddMemberRejectsWhileDeployJobRunning(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&pgsql.Job{
		ID:      testJobID,
		Status:  pgsql.JobStatusRunning,
		Request: pgsql.StoredSpec{ClusterName: "prod", PrimaryIP: "10.0.0.1"},
	})
	_ = store.SaveSecret(testJobID, pgsql.StoredSecret{PostgresUser: "postgres", PostgresPassword: "pw", AdminPassword: "pw"})

	if _, err := svc.AddMember(context.Background(), pgsql.AddMemberRequest{JobID: testJobID, MemberIPs: []string{"10.0.0.5"}}); err == nil {
		t.Fatal("expected AddMember to reject while the deploy job is still running")
	}
}

func TestAddMemberReleasesLockAfterCompletion(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&pgsql.Job{
		ID:      testJobID,
		Status:  pgsql.JobStatusCompleted,
		Request: pgsql.StoredSpec{ClusterName: "prod", PrimaryIP: "10.0.0.1"},
	})
	_ = store.SaveSecret(testJobID, pgsql.StoredSecret{PostgresUser: "postgres", PostgresPassword: "pw", AdminPassword: "pw"})

	if _, err := svc.AddMember(context.Background(), pgsql.AddMemberRequest{JobID: testJobID, MemberIPs: []string{"10.0.0.5"}}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	svc.Wait(context.Background())

	job, err := store.Load(testJobID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if job.ActiveMemberJobID != "" {
		t.Fatalf("expected lock to be released once the member op finished, got %q", job.ActiveMemberJobID)
	}
}

func TestDeployPersistsRunningJobAndSecret(t *testing.T) {
	svc, store := newService(t)
	job, err := svc.Deploy(context.Background(), pgsql.DeployRequest{
		ClusterName:        "prodcluster",
		PrimaryIP:          "10.0.0.1",
		StepTimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	defer svc.Wait(context.Background())

	if job.Status != pgsql.JobStatusRunning || job.ID == "" {
		t.Fatalf("expected a running job with an id, got status=%q id=%q", job.Status, job.ID)
	}
	if _, err := store.Load(job.ID); err != nil {
		t.Fatalf("expected job persisted: %v", err)
	}
	secret, err := svc.GetSecret(job.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if secret.PostgresPassword == "" || secret.ReplicatorPassword == "" {
		t.Fatal("expected postgres and replicator passwords to be generated")
	}
}

func TestGetComputesProgressWithSkippedSteps(t *testing.T) {
	svc, store := newService(t)
	// Empty spec skips standby_config (no standbys) and init_app_db (no user/db):
	// 8 steps - 2 skipped = 6 applicable.
	job := &pgsql.Job{
		ID:     testJobID,
		Status: pgsql.JobStatusRunning,
		Steps: []pgsql.StepResult{
			{Name: "preflight", Status: pgsql.JobStatusCompleted},
			{Name: "standby_config", Status: core.JobStatusSkipped},
		},
	}
	if err := store.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := svc.Get(testJobID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TotalSteps != 6 || got.CompletedSteps != 1 || got.ProgressPercent != 16 {
		t.Fatalf("unexpected progress: total=%d completed=%d pct=%d", got.TotalSteps, got.CompletedSteps, got.ProgressPercent)
	}
}

func TestRecoverCreatesJobFromCompletedDeploy(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&pgsql.Job{
		ID:                testJobID,
		Status:            pgsql.JobStatusCompleted,
		LastCompletedStep: 6,
		Request:           pgsql.StoredSpec{ClusterName: "pg-prod", PrimaryIP: "10.0.0.1"},
	})
	_ = store.SaveSecret(testJobID, pgsql.StoredSecret{
		PostgresPassword:   "pgpw",
		ReplicatorPassword: "replpw",
		AdminPassword:      "adminpw",
	})

	job, err := svc.Recover(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	defer svc.Wait(context.Background())

	if job.Status != pgsql.JobStatusRunning || job.ID == "" || job.ID == testJobID {
		t.Fatalf("expected a new running recovery job, got status=%q id=%q", job.Status, job.ID)
	}
	if job.RecoveryOp == nil || job.RecoveryOp.SourceJobID != testJobID {
		t.Fatalf("expected RecoveryOp.SourceJobID=%q, got %+v", testJobID, job.RecoveryOp)
	}
}

func TestRecoverRejectsRunningJob(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&pgsql.Job{ID: testJobID, Status: pgsql.JobStatusRunning})
	_ = store.SaveSecret(testJobID, pgsql.StoredSecret{PostgresPassword: "pw"})

	if _, err := svc.Recover(context.Background(), testJobID); err == nil {
		t.Fatal("expected Recover to reject a running job")
	}
}

func TestRecoverRejectsRolledBackJob(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&pgsql.Job{ID: testJobID, Status: core.JobStatusRolledBack})
	_ = store.SaveSecret(testJobID, pgsql.StoredSecret{PostgresPassword: "pw"})

	if _, err := svc.Recover(context.Background(), testJobID); err == nil {
		t.Fatal("expected Recover to reject a rolled-back job")
	}
}

func TestStopCreatesJobFromCompletedDeploy(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&pgsql.Job{
		ID:                testJobID,
		Status:            pgsql.JobStatusCompleted,
		LastCompletedStep: 6,
		Request:           pgsql.StoredSpec{ClusterName: "pg-prod", PrimaryIP: "10.0.0.1"},
	})

	job, err := svc.Stop(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	svc.Wait(context.Background())

	if job.ID == "" || job.ID == testJobID {
		t.Fatalf("expected a new stop job, got id=%q", job.ID)
	}
	if job.ServiceOp == nil || job.ServiceOp.Type != "stop" || job.ServiceOp.SourceJobID != testJobID {
		t.Fatalf("expected ServiceOp stop with SourceJobID=%q, got %+v", testJobID, job.ServiceOp)
	}
	// nil runner: the background run fails with "stop runner is not configured",
	// which proves the executor ran and persisted a terminal state.
	final, err := store.Load(job.ID)
	if err != nil {
		t.Fatalf("load stop job: %v", err)
	}
	if final.Status != pgsql.JobStatusFailed {
		t.Fatalf("expected failed stop job with nil runner, got %q", final.Status)
	}
	// The claim on the deploy job must be released when the stop job finishes.
	deploy, _ := store.Load(testJobID)
	if deploy.ActiveMemberJobID != "" {
		t.Fatalf("expected member-op claim released, still held by %q", deploy.ActiveMemberJobID)
	}
}

func TestStopRejectsRunningDeployAndConcurrentOps(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&pgsql.Job{ID: testJobID, Status: pgsql.JobStatusRunning})
	if _, err := svc.Stop(context.Background(), testJobID); err == nil {
		t.Fatal("expected Stop to reject a running deploy job")
	}

	_ = store.Update(testJobID, func(j *pgsql.Job) error {
		j.Status = pgsql.JobStatusCompleted
		j.ActiveMemberJobID = "somememberjob"
		return nil
	})
	if _, err := svc.Stop(context.Background(), testJobID); err == nil {
		t.Fatal("expected Stop to reject while a member operation is running")
	}
}

func TestConnectionInfoFromStoredJob(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&pgsql.Job{ID: testJobID, Status: pgsql.JobStatusCompleted, Request: pgsql.StoredSpec{PrimaryIP: "10.0.0.9", PostgresPort: 5433, StandbyIPs: []string{"10.0.0.10"}}})
	_ = store.SaveSecret(testJobID, pgsql.StoredSecret{PostgresUser: "postgres", PostgresPassword: "pw"})

	host, port, user, pass, nodeIPs, err := svc.ConnectionInfo(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("connection info: %v", err)
	}
	if host != "10.0.0.9" || port != 5433 || user != "postgres" || pass != "pw" {
		t.Fatalf("unexpected connection info: %s:%d %s/%s", host, port, user, pass)
	}
	if len(nodeIPs) != 2 || nodeIPs[0] != "10.0.0.9" {
		t.Fatalf("expected primary + standby node IPs, got %v", nodeIPs)
	}
}

func TestDBManagerRejectsInvalidRequests(t *testing.T) {
	store, err := pgsql.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	db := dbmanager.NewService(store, "")
	ctx := context.Background()

	if err := db.CreateUser(ctx, dbmanager.CreateUserRequest{}); err == nil {
		t.Fatal("expected create-user to require job_id")
	}
	if err := db.CreateDatabase(ctx, dbmanager.CreateDatabaseRequest{JobID: testJobID, DBName: "bad name!"}); err == nil {
		t.Fatal("expected create-database to reject an invalid name")
	}
}

func TestSetConnectionLimitRejectsInvalidRequests(t *testing.T) {
	store, err := pgsql.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	db := dbmanager.NewService(store, "")
	ctx := context.Background()

	cases := map[string]dbmanager.SetConnectionLimitRequest{
		"missing job_id":            {ConnectionLimit: 500},
		"zero limit (deploy-only)":  {JobID: testJobID, ConnectionLimit: 0},
		"connection limit too low":  {JobID: testJobID, ConnectionLimit: 5},
		"connection limit below patroni minimum": {JobID: testJobID, ConnectionLimit: 20},
		"connection limit too high": {JobID: testJobID, ConnectionLimit: 100001},
	}
	for name, req := range cases {
		if status, err := db.SetConnectionLimit(ctx, req); err == nil || status != nil {
			t.Fatalf("expected %s to be rejected before touching any node", name)
		}
	}

	// Valid request against a job that does not exist must fail on job lookup.
	if _, err := db.SetConnectionLimit(ctx, dbmanager.SetConnectionLimitRequest{JobID: testJobID, ConnectionLimit: 500}); err == nil {
		t.Fatal("expected set-connection-limit on an unknown job to fail")
	}
	if _, err := db.GetConnectionLimit(ctx, testJobID); err == nil {
		t.Fatal("expected get-connection-limit on an unknown job to fail")
	}
}
