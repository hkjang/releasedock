package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hkjang/releasedock/runner/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
)

type recordingLeaseExecutor struct {
	tag       pgconn.CommandTag
	err       error
	statement string
	arguments []any
}

func (e *recordingLeaseExecutor) Exec(_ context.Context, statement string, arguments ...any) (pgconn.CommandTag, error) {
	e.statement = statement
	e.arguments = append([]any(nil), arguments...)
	return e.tag, e.err
}

func TestLeaseMutationRejectsZeroAffectedRows(t *testing.T) {
	executor := &recordingLeaseExecutor{tag: pgconn.NewCommandTag("UPDATE 0")}
	err := executeLeaseMutation(context.Background(), executor, "test mutation", "UPDATE guarded", "job", "worker")
	if !errors.Is(err, ErrLostLease) {
		t.Fatalf("zero-row guarded mutation = %v, want ErrLostLease", err)
	}
	if executor.statement != "UPDATE guarded" || len(executor.arguments) != 2 {
		t.Fatalf("mutation was not forwarded intact: %q %#v", executor.statement, executor.arguments)
	}
}

func TestLeaseMutationAcceptsExactlyOneAffectedRow(t *testing.T) {
	executor := &recordingLeaseExecutor{tag: pgconn.NewCommandTag("INSERT 0 1")}
	if err := executeLeaseMutation(context.Background(), executor, "test mutation", "INSERT guarded"); err != nil {
		t.Fatal(err)
	}
}

func TestPostStepMutationsRequireCurrentJobAndReleaseLock(t *testing.T) {
	for name, statement := range map[string]string{
		"finish step":  finishStepSQL,
		"append log":   appendLogSQL,
		"record image": recordImageSQL,
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{
				"locked_by", "release_locks", "l.worker_id",
				"j.status NOT IN ('SUCCESS', 'FAILED', 'ROLLED_BACK')",
			} {
				if !strings.Contains(statement, required) {
					t.Errorf("lease-guarded SQL lacks %q:\n%s", required, statement)
				}
			}
		})
	}
}

func TestValidateJobRejectsAutomaticRollback(t *testing.T) {
	err := validateJob(&model.Job{Profile: model.Profile{AutoRollback: true}})
	if err == nil || !strings.Contains(err.Error(), "automatic rollback is disabled") {
		t.Fatalf("unsafe automatic rollback was accepted: %v", err)
	}
}
