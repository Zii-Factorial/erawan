package haproxy

import (
	"context"
	"errors"
	"testing"
)

type fakeReleaser struct {
	called []string
	err    error
}

func (f *fakeReleaser) ReleaseDCS(_ context.Context, jobID string) (string, error) {
	f.called = append(f.called, jobID)
	if f.err != nil {
		return "", f.err
	}
	return "release-" + jobID, nil
}

// Deleting a cluster's proxy config is how a cluster is decommissioned, so the
// tenant it holds on the shared control plane has to go with it.
func TestDeleteReleasesTheClustersTenant(t *testing.T) {
	f := &fakeReleaser{}
	h := New(nil, f)

	got := h.releaseTenant(context.Background(), "job-abc")

	if len(f.called) != 1 || f.called[0] != "job-abc" {
		t.Fatalf("released %v, want [job-abc]", f.called)
	}
	if got["control_plane"] != "releasing" {
		t.Errorf("status = %v, want releasing", got["control_plane"])
	}
	if got["release_job_id"] != "release-job-abc" {
		t.Errorf("release job = %v", got["release_job_id"])
	}
}

// Without an explicit job ID the cluster is unknown, and it must stay unknown:
// node IPs are recycled, so inferring one from the config's backends can point
// at a DIFFERENT, live cluster — and releasing that tenant deletes its Patroni
// keys and takes it down. Leaking state is recoverable; this is not.
func TestDeleteNeverGuessesWhichTenantToRelease(t *testing.T) {
	f := &fakeReleaser{}
	h := New(nil, f)

	got := h.releaseTenant(context.Background(), "")

	if len(f.called) != 0 {
		t.Fatalf("released %v with no job_id supplied", f.called)
	}
	if got["control_plane"] == "releasing" {
		t.Error("reported a release that never happened")
	}
}

// A control-plane failure must be visible in the response. The proxy config is
// already gone by this point, so reporting silence here is what leaves a live
// credential and an open client-port grant behind with nobody aware of it.
func TestReleaseFailureIsReportedNotSwallowed(t *testing.T) {
	f := &fakeReleaser{err: errors.New("control plane unreachable")}
	h := New(nil, f)

	got := h.releaseTenant(context.Background(), "job-abc")

	status, _ := got["control_plane"].(string)
	if status == "releasing" {
		t.Fatal("a failed release reported as success")
	}
	if !contains(status, "control plane unreachable") {
		t.Errorf("status %q does not carry the cause", status)
	}
	if _, ok := got["release_job_id"]; ok {
		t.Error("release_job_id present despite failure")
	}
}

// A deployment with no control plane configured must still delete its config.
func TestNoControlPlaneConfiguredIsSkippedNotAnError(t *testing.T) {
	h := New(nil, nil)

	got := h.releaseTenant(context.Background(), "job-abc")

	status, _ := got["control_plane"].(string)
	if !contains(status, "skipped") {
		t.Errorf("status = %q, want a skip", status)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
