package pgsql

import "testing"

// The single-job form is what every existing caller sends; it must keep working
// unchanged, with no confirm flag.
func TestSingleJobIDNeedsNoConfirmation(t *testing.T) {
	got, err := releaseDCSRequest{JobID: "job-a"}.jobIDs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "job-a" {
		t.Fatalf("got %v, want [job-a]", got)
	}
}

// Releasing several clusters at once deletes several clusters' Patroni state,
// so it must not happen because a list arrived by accident.
func TestBatchRequiresConfirmation(t *testing.T) {
	if _, err := (releaseDCSRequest{JobIDs: []string{"a", "b"}}).jobIDs(); err == nil {
		t.Fatal("batch accepted without confirm")
	}
	got, err := releaseDCSRequest{JobIDs: []string{"a", "b"}, Confirm: true}.jobIDs()
	if err != nil {
		t.Fatalf("confirmed batch rejected: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want two", got)
	}
}

// Both shapes may be sent together; the same cluster must not be released twice.
func TestBothFormsMergeAndDeduplicate(t *testing.T) {
	got, err := releaseDCSRequest{JobID: "a", JobIDs: []string{"a", "b", "b"}, Confirm: true}.jobIDs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v, want [a b]", got)
	}
}

// An empty body must be refused rather than treated as "release everything".
func TestEmptyBodyIsRefused(t *testing.T) {
	for _, req := range []releaseDCSRequest{
		{},
		{Confirm: true},
		{JobID: "   "},
		{JobIDs: []string{"", "  "}, Confirm: true},
	} {
		if _, err := req.jobIDs(); err == nil {
			t.Errorf("%+v was accepted", req)
		}
	}
}

// Whitespace around an ID must not produce a second, unmatchable entry.
func TestIDsAreTrimmed(t *testing.T) {
	got, err := releaseDCSRequest{JobIDs: []string{" a ", "a"}, Confirm: true}.jobIDs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("got %v, want [a]", got)
	}
}
