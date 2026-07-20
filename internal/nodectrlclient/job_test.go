package nodectrlclient

import (
	"errors"
	"strings"
	"testing"
	"time"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/nodejob"
)

const testJobID = "job"

type fakeJobManager struct {
	startReq nodejob.StartRequest
	getID    string
	cancelID string
	deleteID string
	snapshot nodejob.Snapshot
	err      error
}

func (m *fakeJobManager) Start(req nodejob.StartRequest) (nodejob.Snapshot, error) {
	m.startReq = req
	return m.snapshot, m.err
}

func (m *fakeJobManager) Get(id string, _, _ uint64, _ int) (nodejob.Snapshot, error) {
	m.getID = id
	return m.snapshot, m.err
}

func (m *fakeJobManager) Cancel(id string) (nodejob.Snapshot, error) {
	m.cancelID = id
	return m.snapshot, m.err
}

func (m *fakeJobManager) Delete(id string) error {
	m.deleteID = id
	return m.err
}

func TestExecuteJobControlStartAndSnapshot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	m := &fakeJobManager{snapshot: nodejob.Snapshot{
		ID: "job-1", SandboxUID: "sb", State: nodejob.StateFailed,
		Stdout:   nodejob.Chunk{Data: []byte("out"), NextCursor: 7, CursorExpired: true},
		Stderr:   nodejob.Chunk{Data: []byte("err"), NextCursor: 8},
		ExitCode: 2, ExitCodeSet: true, Reason: "failed", CreatedAt: now,
		StartedAt: now.Add(time.Second), CompletedAt: now.Add(2 * time.Second), CancelCalled: true,
	}}
	req := &nodectrlv1.JobControlRequest{RequestId: "request-1", Operation: &nodectrlv1.JobControlRequest_Start{
		Start: &nodectrlv1.JobStartRequest{IdempotencyKey: "run-1", SandboxUid: "sb", Command: []string{"agent", "go"}},
	}}
	resp := executeJobControl(req, m)
	if resp.GetErrorCode() != nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_UNSPECIFIED || resp.GetRequestId() != "request-1" {
		t.Fatalf("response = %+v", resp)
	}
	if m.startReq.IdempotencyKey != "run-1" || m.startReq.SandboxUID != "sb" || strings.Join(m.startReq.Command, " ") != "agent go" {
		t.Fatalf("start request = %+v", m.startReq)
	}
	s := resp.GetSnapshot()
	if s.GetJobId() != "job-1" || s.GetState() != nodectrlv1.JobState_JOB_STATE_FAILED ||
		string(s.GetStdout().GetData()) != "out" || !s.GetStdout().GetCursorExpired() ||
		s.ExitCode == nil || s.GetExitCode() != 2 || s.GetCreatedAt().AsTime() != now || !s.GetCancelCalled() {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestExecuteJobControlGetCancelDelete(t *testing.T) {
	m := &fakeJobManager{snapshot: nodejob.Snapshot{ID: testJobID}}
	get := &nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Get{
		Get: &nodectrlv1.JobGetRequest{JobId: testJobID},
	}}
	if got := executeJobControl(get, m); got.GetSnapshot().GetJobId() != testJobID || m.getID != testJobID {
		t.Fatalf("Get response=%+v id=%q", got, m.getID)
	}
	cancel := &nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Cancel{
		Cancel: &nodectrlv1.JobCancelRequest{JobId: testJobID},
	}}
	if got := executeJobControl(cancel, m); got.GetSnapshot() == nil || m.cancelID != testJobID {
		t.Fatalf("Cancel response=%+v id=%q", got, m.cancelID)
	}
	del := &nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Delete{
		Delete: &nodectrlv1.JobDeleteRequest{JobId: testJobID},
	}}
	if got := executeJobControl(del, m); got.GetSnapshot() != nil || got.GetError() != "" || m.deleteID != testJobID {
		t.Fatalf("Delete response=%+v id=%q", got, m.deleteID)
	}
}

func TestExecuteJobControlStructuredErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code nodectrlv1.JobControlErrorCode
	}{
		{"not found", nodejob.ErrNotFound, nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_FOUND},
		{"capacity", nodejob.ErrCapacity, nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_CAPACITY},
		{"cursor", nodejob.ErrInvalidCursor, nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INVALID_CURSOR},
		{"terminal", nodejob.ErrNotTerminal, nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_TERMINAL},
		{"conflict", nodejob.ErrIdempotencyConflict, nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_IDEMPOTENCY_CONFLICT},
		{"argument", errors.New("bad request"), nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INVALID_ARGUMENT},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &fakeJobManager{err: tc.err}
			req := &nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Get{Get: &nodectrlv1.JobGetRequest{JobId: "x"}}}
			if got := executeJobControl(req, m); got.GetErrorCode() != tc.code {
				t.Fatalf("error code = %s, want %s", got.GetErrorCode(), tc.code)
			}
		})
	}
	if got := executeJobControl(&nodectrlv1.JobControlRequest{}, nil); got.GetErrorCode() != nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INTERNAL {
		t.Fatalf("nil manager = %+v", got)
	}
	long := &fakeJobManager{err: errors.New(strings.Repeat("x", maxJobControlErrorBytes+10))}
	got := executeJobControl(&nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Get{
		Get: &nodectrlv1.JobGetRequest{JobId: "x"},
	}}, long)
	if len(got.GetError()) != maxJobControlErrorBytes {
		t.Fatalf("error len = %d", len(got.GetError()))
	}
}

func TestExecuteJobControlRejectsNilOperations(t *testing.T) {
	m := &fakeJobManager{}
	requests := []*nodectrlv1.JobControlRequest{
		{Operation: &nodectrlv1.JobControlRequest_Start{}},
		{Operation: &nodectrlv1.JobControlRequest_Get{}},
		{Operation: &nodectrlv1.JobControlRequest_Cancel{}},
		{Operation: &nodectrlv1.JobControlRequest_Delete{}},
		{},
	}
	for i, req := range requests {
		got := executeJobControl(req, m)
		if got.GetErrorCode() != nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INVALID_ARGUMENT {
			t.Errorf("request %d = %+v", i, got)
		}
	}
	m.err = nodejob.ErrNotTerminal
	del := &nodectrlv1.JobControlRequest{Operation: &nodectrlv1.JobControlRequest_Delete{
		Delete: &nodectrlv1.JobDeleteRequest{JobId: testJobID},
	}}
	if got := executeJobControl(del, m); got.GetErrorCode() != nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_TERMINAL {
		t.Fatalf("Delete error = %+v", got)
	}
}

func TestJobStateToProtoAndZeroTimestamps(t *testing.T) {
	states := map[nodejob.State]nodectrlv1.JobState{
		nodejob.StateStarting:  nodectrlv1.JobState_JOB_STATE_STARTING,
		nodejob.StateRunning:   nodectrlv1.JobState_JOB_STATE_RUNNING,
		nodejob.StateCanceling: nodectrlv1.JobState_JOB_STATE_CANCELING,
		nodejob.StateSucceeded: nodectrlv1.JobState_JOB_STATE_SUCCEEDED,
		nodejob.StateFailed:    nodectrlv1.JobState_JOB_STATE_FAILED,
		nodejob.StateCanceled:  nodectrlv1.JobState_JOB_STATE_CANCELED,
		"unknown":              nodectrlv1.JobState_JOB_STATE_UNSPECIFIED,
	}
	for in, want := range states {
		if got := jobStateToProto(in); got != want {
			t.Errorf("state %q = %s, want %s", in, got, want)
		}
	}
	if got := jobSnapshotToProto(nodejob.Snapshot{}); got.GetCreatedAt() != nil || got.ExitCode != nil {
		t.Fatalf("zero snapshot = %+v", got)
	}
}
