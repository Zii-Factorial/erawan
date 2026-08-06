// Package mysql_test holds black-box unit tests for the MySQL cluster service
// and its database manager, exercising only their exported API.
package mysql_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"erawan-cluster/internal/cluster/core"
	mysql "erawan-cluster/internal/cluster/mysql"
	dbmanager "erawan-cluster/internal/cluster/mysql/dbmanager"
)

func tempKey(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(p, []byte("test-key"), 0o600); err != nil {
		t.Fatalf("write temp key: %v", err)
	}
	return p
}

func newService(t *testing.T) (*mysql.Service, mysql.Store) {
	t.Helper()
	store, err := mysql.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	svc := mysql.NewService(store, nil) // nil runner: no real ansible is executed
	if err := svc.SetSSHConfig("clusterops", tempKey(t)); err != nil {
		t.Fatalf("set ssh config: %v", err)
	}
	return svc, store
}

func TestValidateDeployRequestAppliesDefaults(t *testing.T) {
	req := mysql.DeployRequest{ClusterName: "prodCluster", PrimaryIP: "10.0.0.1"}
	if err := mysql.ValidateDeployRequest(&req); err != nil {
		t.Fatalf("expected valid request, got %v", err)
	}
	if req.SSHPort != 22 || req.MySQLPort != 3306 || req.MySQLVersion != 8 {
		t.Fatalf("unexpected defaults: ssh=%d mysql=%d ver=%d", req.SSHPort, req.MySQLPort, req.MySQLVersion)
	}
	if req.AdminUsername != "clusteradmin" {
		t.Fatalf("expected default admin username, got %q", req.AdminUsername)
	}
	if req.ConnectionLimit != 0 {
		t.Fatalf("expected connection_limit to default to 0 (engine default), got %d", req.ConnectionLimit)
	}

	withLimit := mysql.DeployRequest{ClusterName: "prodCluster", PrimaryIP: "10.0.0.1", ConnectionLimit: 500}
	if err := mysql.ValidateDeployRequest(&withLimit); err != nil {
		t.Fatalf("expected connection_limit 500 to be valid, got %v", err)
	}
}

func TestValidateDeployRequestRejectsBadInput(t *testing.T) {
	cases := map[string]mysql.DeployRequest{
		"missing cluster name":      {PrimaryIP: "10.0.0.1"},
		"bad primary ip":            {ClusterName: "c", PrimaryIP: "not-an-ip"},
		"bad standby ip":            {ClusterName: "c", PrimaryIP: "10.0.0.1", StandbyIPs: []string{"x"}},
		"connection limit too low":  {ClusterName: "prodCluster", PrimaryIP: "10.0.0.1", ConnectionLimit: 5},
		"connection limit too high": {ClusterName: "prodCluster", PrimaryIP: "10.0.0.1", ConnectionLimit: 100001},
	}
	for name, req := range cases {
		req := req
		t.Run(name, func(t *testing.T) {
			if err := mysql.ValidateDeployRequest(&req); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

const testJobID = "aabbccddee1122334455aabb"

func TestValidateMemberRequests(t *testing.T) {
	if err := mysql.ValidateAddMemberRequest(&mysql.AddMemberRequest{}); err == nil {
		t.Fatal("expected error for empty add-member request")
	}
	add := &mysql.AddMemberRequest{JobID: testJobID, MemberIPs: []string{"10.0.0.5"}}
	if err := mysql.ValidateAddMemberRequest(add); err != nil {
		t.Fatalf("expected valid add-member request, got %v", err)
	}
	if err := mysql.ValidateRemoveMemberRequest(&mysql.RemoveMemberRequest{JobID: testJobID, MemberIP: "bad"}); err == nil {
		t.Fatal("expected error for invalid remove-member IP")
	}
}

func TestSetSSHConfigRejectsInvalidUser(t *testing.T) {
	svc := mysql.NewService(nil, nil)
	if err := svc.SetSSHConfig("bad user!", tempKey(t)); err == nil {
		t.Fatal("expected invalid ssh user to be rejected")
	}
}

// Two add/remove-member calls racing against the same cluster both mutate
// Group Replication membership, which can transiently break quorum on the
// primary. AddMember must reject a second call while one is already in
// flight rather than let them race inside Ansible.
func TestAddMemberRejectsWhileAnotherMemberOpRunning(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{
		ID:                testJobID,
		Status:            mysql.JobStatusCompleted,
		Request:           mysql.StoredSpec{ClusterName: "prod", PrimaryIP: "10.0.0.1"},
		ActiveMemberJobID: "already-running-job",
	})
	_ = store.SaveSecret(testJobID, mysql.StoredSecret{AdminUser: "admin", AdminPassword: "pw"})

	if _, err := svc.AddMember(context.Background(), mysql.AddMemberRequest{JobID: testJobID, MemberIPs: []string{"10.0.0.5"}}); err == nil {
		t.Fatal("expected AddMember to reject when another member operation is already running")
	}
}

func TestAddMemberRejectsWhileDeployJobRunning(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{
		ID:      testJobID,
		Status:  mysql.JobStatusRunning,
		Request: mysql.StoredSpec{ClusterName: "prod", PrimaryIP: "10.0.0.1"},
	})
	_ = store.SaveSecret(testJobID, mysql.StoredSecret{AdminUser: "admin", AdminPassword: "pw"})

	if _, err := svc.AddMember(context.Background(), mysql.AddMemberRequest{JobID: testJobID, MemberIPs: []string{"10.0.0.5"}}); err == nil {
		t.Fatal("expected AddMember to reject while the deploy job is still running")
	}
}

func TestAddMemberReleasesLockAfterCompletion(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{
		ID:      testJobID,
		Status:  mysql.JobStatusCompleted,
		Request: mysql.StoredSpec{ClusterName: "prod", PrimaryIP: "10.0.0.1"},
	})
	_ = store.SaveSecret(testJobID, mysql.StoredSecret{AdminUser: "admin", AdminPassword: "pw"})

	if _, err := svc.AddMember(context.Background(), mysql.AddMemberRequest{JobID: testJobID, MemberIPs: []string{"10.0.0.5"}}); err != nil {
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
	job, err := svc.Deploy(context.Background(), mysql.DeployRequest{
		ClusterName:        "prodCluster",
		PrimaryIP:          "10.0.0.1",
		StepTimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	defer svc.Wait(context.Background()) // drain the background goroutine

	if job.Status != mysql.JobStatusRunning || job.ID == "" {
		t.Fatalf("expected a running job with an id, got status=%q id=%q", job.Status, job.ID)
	}
	if _, err := store.Load(job.ID); err != nil {
		t.Fatalf("expected job persisted: %v", err)
	}
	secret, err := svc.GetSecret(job.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	if secret.AdminPassword == "" {
		t.Fatal("expected an admin password to be generated and stored")
	}
}

func TestDeployRejectsInvalidRequest(t *testing.T) {
	svc, _ := newService(t)
	if _, err := svc.Deploy(context.Background(), mysql.DeployRequest{}); err == nil {
		t.Fatal("expected deploy to reject an invalid request")
	}
}

func TestGetComputesProgressWithSkippedSteps(t *testing.T) {
	svc, store := newService(t)
	job := &mysql.Job{
		ID:     testJobID,
		Status: mysql.JobStatusRunning,
		Request: mysql.StoredSpec{
			AssumePrepared: true,
		},
		Steps: []mysql.StepResult{
			{Name: "preflight", Status: core.JobStatusSkipped},
			{Name: "configure_instances", Status: core.JobStatusSkipped},
			{Name: "create_cluster", Status: mysql.JobStatusCompleted},
			{Name: "add_instances", Status: core.JobStatusSkipped},
		},
	}
	if err := store.Save(job); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := svc.Get(testJobID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TotalSteps != 5 || got.CompletedSteps != 1 || got.ProgressPercent != 20 {
		t.Fatalf("unexpected progress: total=%d completed=%d pct=%d", got.TotalSteps, got.CompletedSteps, got.ProgressPercent)
	}
}

func TestRecoverCreatesJobFromCompletedDeploy(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{
		ID:                testJobID,
		Status:            mysql.JobStatusCompleted,
		LastCompletedStep: 8,
		Request:           mysql.StoredSpec{ClusterName: "prod", PrimaryIP: "10.0.0.1"},
	})
	_ = store.SaveSecret(testJobID, mysql.StoredSecret{AdminUser: "admin", AdminPassword: "pw"})

	job, err := svc.Recover(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	defer svc.Wait(context.Background())

	if job.Status != mysql.JobStatusRunning || job.ID == "" || job.ID == testJobID {
		t.Fatalf("expected a new running recovery job, got status=%q id=%q", job.Status, job.ID)
	}
	if job.RecoveryOp == nil || job.RecoveryOp.SourceJobID != testJobID {
		t.Fatalf("expected RecoveryOp.SourceJobID=%q, got %+v", testJobID, job.RecoveryOp)
	}
}

func TestRecoverWorksOnFailedJob(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{
		ID:      testJobID,
		Status:  mysql.JobStatusFailed,
		Request: mysql.StoredSpec{ClusterName: "prod", PrimaryIP: "10.0.0.1"},
	})
	_ = store.SaveSecret(testJobID, mysql.StoredSecret{AdminUser: "admin", AdminPassword: "pw"})

	job, err := svc.Recover(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("recover on failed job: %v", err)
	}
	defer svc.Wait(context.Background())
	if job.RecoveryOp == nil {
		t.Fatal("expected RecoveryOp set on recovery job")
	}
}

func TestRecoverRejectsRunningJob(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{ID: testJobID, Status: mysql.JobStatusRunning})
	_ = store.SaveSecret(testJobID, mysql.StoredSecret{AdminPassword: "pw"})

	if _, err := svc.Recover(context.Background(), testJobID); err == nil {
		t.Fatal("expected Recover to reject a running job")
	}
}

func TestRecoverRejectsRolledBackJob(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{ID: testJobID, Status: mysql.JobStatusRolledBack})
	_ = store.SaveSecret(testJobID, mysql.StoredSecret{AdminPassword: "pw"})

	if _, err := svc.Recover(context.Background(), testJobID); err == nil {
		t.Fatal("expected Recover to reject a rolled-back job")
	}
}

func TestStopCreatesJobFromCompletedDeploy(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{
		ID:                testJobID,
		Status:            mysql.JobStatusCompleted,
		LastCompletedStep: 8,
		Request:           mysql.StoredSpec{ClusterName: "mysql-prod", PrimaryIP: "10.0.0.1"},
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
	if final.Status != mysql.JobStatusFailed {
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
	_ = store.Save(&mysql.Job{ID: testJobID, Status: mysql.JobStatusRunning})
	if _, err := svc.Stop(context.Background(), testJobID); err == nil {
		t.Fatal("expected Stop to reject a running deploy job")
	}

	_ = store.Update(testJobID, func(j *mysql.Job) error {
		j.Status = mysql.JobStatusCompleted
		j.ActiveMemberJobID = "somememberjob"
		return nil
	})
	if _, err := svc.Stop(context.Background(), testJobID); err == nil {
		t.Fatal("expected Stop to reject while a member operation is running")
	}
}

func TestListReturnsSeededJobs(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{ID: "a", Status: mysql.JobStatusCompleted})
	_ = store.Save(&mysql.Job{ID: "b", Status: mysql.JobStatusCompleted})
	jobs, err := svc.List(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
}

func TestConnectionInfoFromStoredJob(t *testing.T) {
	svc, store := newService(t)
	_ = store.Save(&mysql.Job{ID: testJobID, Status: mysql.JobStatusCompleted, Request: mysql.StoredSpec{PrimaryIP: "10.0.0.9", MySQLPort: 3307}})
	_ = store.SaveSecret(testJobID, mysql.StoredSecret{AdminUser: "clusteradmin", AdminPassword: "pw"})

	host, port, user, pass, nodeIPs, err := svc.ConnectionInfo(testJobID)
	if err != nil {
		t.Fatalf("connection info: %v", err)
	}
	if host != "10.0.0.9" || port != 3307 || user != "clusteradmin" || pass != "pw" {
		t.Fatalf("unexpected connection info: %s:%d %s/%s", host, port, user, pass)
	}
	_ = nodeIPs
}

// ── database manager (validation through the public API) ─────────────────────

func TestDBManagerRejectsInvalidRequests(t *testing.T) {
	store, err := mysql.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	db := dbmanager.NewService(store, "")
	ctx := context.Background()

	if err := db.CreateUser(ctx, dbmanager.CreateUserRequest{}); err == nil {
		t.Fatal("expected create-user to require job_id")
	}
	if err := db.CreateUser(ctx, dbmanager.CreateUserRequest{JobID: testJobID, Username: "ok"}); err == nil {
		t.Fatal("expected create-user to require a password")
	}
	if err := db.CreateDatabase(ctx, dbmanager.CreateDatabaseRequest{JobID: testJobID, DBName: "bad name!"}); err == nil {
		t.Fatal("expected create-database to reject an invalid name")
	}
	if err := db.DeleteUser(ctx, dbmanager.DeleteUserRequest{JobID: testJobID, Username: "bad user!"}); err == nil {
		t.Fatal("expected delete-user to reject an invalid username")
	}
}

func TestSetConnectionLimitRejectsInvalidRequests(t *testing.T) {
	store, err := mysql.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	db := dbmanager.NewService(store, "")
	ctx := context.Background()

	cases := map[string]dbmanager.SetConnectionLimitRequest{
		"missing job_id":            {ConnectionLimit: 500},
		"zero limit (deploy-only)":  {JobID: testJobID, ConnectionLimit: 0},
		"connection limit too low":  {JobID: testJobID, ConnectionLimit: 5},
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
