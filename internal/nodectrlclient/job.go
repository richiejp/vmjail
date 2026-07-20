package nodectrlclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/nodejob"
)

const maxJobControlErrorBytes = 16 << 10

func (c *Client) handleJobControl(ctx context.Context, req *nodectrlv1.JobControlRequest) error {
	resp := executeJobControl(req, c.cfg.JobManager)
	_, err := c.ctrl.ReportJobControl(ctx, connect.NewRequest(resp))
	if err != nil {
		return fmt.Errorf("report response %q: %w", req.GetRequestId(), err)
	}
	return nil
}

func executeJobControl(req *nodectrlv1.JobControlRequest, manager JobManager) *nodectrlv1.JobControlResponse {
	resp := &nodectrlv1.JobControlResponse{RequestId: req.GetRequestId()}
	if manager == nil {
		return jobError(resp, nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INTERNAL,
			errors.New("node detached-job manager is unavailable"))
	}
	var (
		snapshot nodejob.Snapshot
		err      error
	)
	switch op := req.GetOperation().(type) {
	case *nodectrlv1.JobControlRequest_Start:
		if op.Start == nil {
			err = errors.New("nil start request")
			break
		}
		snapshot, err = manager.Start(nodejob.StartRequest{
			IdempotencyKey: op.Start.GetIdempotencyKey(),
			SandboxUID:     op.Start.GetSandboxUid(),
			Command:        append([]string(nil), op.Start.GetCommand()...),
		})
	case *nodectrlv1.JobControlRequest_Get:
		if op.Get == nil {
			err = errors.New("nil get request")
			break
		}
		snapshot, err = manager.Get(op.Get.GetJobId(), op.Get.GetStdoutCursor(),
			op.Get.GetStderrCursor(), int(op.Get.GetMaxBytesPerStream()))
	case *nodectrlv1.JobControlRequest_Cancel:
		if op.Cancel == nil {
			err = errors.New("nil cancel request")
			break
		}
		snapshot, err = manager.Cancel(op.Cancel.GetJobId())
	case *nodectrlv1.JobControlRequest_Delete:
		if op.Delete == nil {
			err = errors.New("nil delete request")
			break
		}
		err = manager.Delete(op.Delete.GetJobId())
		if err == nil {
			return resp
		}
	default:
		err = errors.New("missing job operation")
	}
	if err != nil {
		return jobError(resp, jobErrorCode(err), err)
	}
	resp.Snapshot = jobSnapshotToProto(snapshot)
	return resp
}

func jobError(resp *nodectrlv1.JobControlResponse, code nodectrlv1.JobControlErrorCode, err error) *nodectrlv1.JobControlResponse {
	resp.ErrorCode = code
	resp.Error = err.Error()
	if len(resp.Error) > maxJobControlErrorBytes {
		resp.Error = resp.Error[:maxJobControlErrorBytes]
	}
	return resp
}

func jobErrorCode(err error) nodectrlv1.JobControlErrorCode {
	switch {
	case errors.Is(err, nodejob.ErrNotFound):
		return nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_FOUND
	case errors.Is(err, nodejob.ErrCapacity):
		return nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_CAPACITY
	case errors.Is(err, nodejob.ErrInvalidCursor):
		return nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INVALID_CURSOR
	case errors.Is(err, nodejob.ErrNotTerminal):
		return nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_NOT_TERMINAL
	case errors.Is(err, nodejob.ErrIdempotencyConflict):
		return nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_IDEMPOTENCY_CONFLICT
	default:
		return nodectrlv1.JobControlErrorCode_JOB_CONTROL_ERROR_CODE_INVALID_ARGUMENT
	}
}

func jobSnapshotToProto(s nodejob.Snapshot) *nodectrlv1.JobSnapshot {
	out := &nodectrlv1.JobSnapshot{
		JobId: s.ID, SandboxUid: s.SandboxUID, State: jobStateToProto(s.State),
		Stdout: &nodectrlv1.JobOutputChunk{Data: s.Stdout.Data, NextCursor: s.Stdout.NextCursor, CursorExpired: s.Stdout.CursorExpired},
		Stderr: &nodectrlv1.JobOutputChunk{Data: s.Stderr.Data, NextCursor: s.Stderr.NextCursor, CursorExpired: s.Stderr.CursorExpired},
		Reason: s.Reason, CancelCalled: s.CancelCalled,
		CreatedAt: timestampOrNil(s.CreatedAt), StartedAt: timestampOrNil(s.StartedAt), CompletedAt: timestampOrNil(s.CompletedAt),
	}
	if s.ExitCodeSet {
		out.ExitCode = &s.ExitCode
	}
	return out
}

func timestampOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func jobStateToProto(state nodejob.State) nodectrlv1.JobState {
	switch state {
	case nodejob.StateStarting:
		return nodectrlv1.JobState_JOB_STATE_STARTING
	case nodejob.StateRunning:
		return nodectrlv1.JobState_JOB_STATE_RUNNING
	case nodejob.StateCanceling:
		return nodectrlv1.JobState_JOB_STATE_CANCELING
	case nodejob.StateSucceeded:
		return nodectrlv1.JobState_JOB_STATE_SUCCEEDED
	case nodejob.StateFailed:
		return nodectrlv1.JobState_JOB_STATE_FAILED
	case nodejob.StateCanceled:
		return nodectrlv1.JobState_JOB_STATE_CANCELED
	default:
		return nodectrlv1.JobState_JOB_STATE_UNSPECIFIED
	}
}
