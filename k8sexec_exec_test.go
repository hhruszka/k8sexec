package k8sexec

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/exec"
)

// The exec path cannot use fake.NewSimpleClientset alone: the fake clientset does not
// implement the exec subresource, and remotecommand needs a real *rest.Config to build
// an executor against. These helpers point that config at an httptest server that never
// completes a protocol upgrade, which is what drives every failure branch.
//
// This covers the transport failures the (record, error) split exists to distinguish.
// The success path needs a server speaking SPDY or WebSocket streaming and is out of
// reach here; if it becomes necessary, the executor construction in exec would have to
// be extracted behind an injectable factory.

// newExecTarget returns a K8SExec whose exec requests reach a server that rejects the
// upgrade. The clientset must be a real one built from the same config: exec goes
// through Clientset.CoreV1().RESTClient(), which the fake clientset returns as nil.
func newExecTarget(t *testing.T, handler http.HandlerFunc) *K8SExec {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	config := &rest.Config{Host: server.URL}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("building clientset for test server: %v", err)
	}

	return &K8SExec{Config: config, Clientset: clientset}
}

// refuseUpgrade responds without switching protocols, so the stream never establishes.
func refuseUpgrade(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "upgrade not supported", http.StatusInternalServerError)
}

// TestExecWithContextTransportFailureReturnsNoRecord pins the core contract: when no
// exit status is obtained there is no record to speak of, and the error identifies the
// class. A record here would be indistinguishable from a command that ran and failed.
func TestExecWithContextTransportFailureReturnsNoRecord(t *testing.T) {
	k8s := newExecTarget(t, refuseUpgrade)

	record, err := k8s.ExecWithContext(context.Background(), "ns", "pod", "container", []string{"true"}, nil)

	if err == nil {
		t.Fatal("ExecWithContext() error = nil, want a transport failure")
	}
	if record != nil {
		t.Errorf("ExecWithContext() record = %+v, want nil when nothing ran", record)
	}
	if !errors.Is(err, ErrNotExecuted) {
		t.Errorf("error %v does not match ErrNotExecuted", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Errorf("error %v matched ErrTimeout, but no deadline fired", err)
	}
}

// TestExecWithContextErrorNamesTheTarget checks the wrapped message identifies which
// container failed, since a caller holding only the error has lost that context.
func TestExecWithContextErrorNamesTheTarget(t *testing.T) {
	k8s := newExecTarget(t, refuseUpgrade)

	_, err := k8s.ExecWithContext(context.Background(), "prod", "web-0", "sidecar", []string{"true"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}

	for _, want := range []string{"prod", "web-0", "sidecar"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestExecWithContextCancelledContext covers a deadline that has already expired: the
// error must classify as a timeout and, because ErrTimeout wraps ErrNotExecuted, also
// satisfy the broader "nothing ran" test.
func TestExecWithContextCancelledContext(t *testing.T) {
	k8s := newExecTarget(t, func(w http.ResponseWriter, r *http.Request) {
		// Outlast the client's deadline so the context governs the outcome.
		time.Sleep(200 * time.Millisecond)
		refuseUpgrade(w, r)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	record, err := k8s.ExecWithContext(ctx, "ns", "pod", "container", []string{"sleep", "60"}, nil)

	if err == nil {
		t.Fatal("ExecWithContext() error = nil, want a timeout")
	}
	if record != nil {
		t.Errorf("ExecWithContext() record = %+v, want nil on timeout", record)
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("error %v does not match ErrTimeout", err)
	}
	// The wrapping relationship is load-bearing: callers that only ask "did this
	// produce a result?" must catch timeouts without testing for them specifically.
	if !errors.Is(err, ErrNotExecuted) {
		t.Errorf("ErrTimeout must wrap ErrNotExecuted; %v did not match", err)
	}
}

// TestExecTimeoutIsClassifiedAsTimeout covers the same path through Exec, which builds
// its own context from the duration.
func TestExecTimeoutIsClassifiedAsTimeout(t *testing.T) {
	k8s := newExecTarget(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		refuseUpgrade(w, r)
	})

	record, err := k8s.Exec("ns", "pod", "container", []string{"sleep", "60"}, nil, 10*time.Millisecond)

	if err == nil {
		t.Fatal("Exec() error = nil, want a timeout")
	}
	if record != nil {
		t.Errorf("Exec() record = %+v, want nil on timeout", record)
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("error %v does not match ErrTimeout", err)
	}
}

// TestClassifyNonZeroExitIsNotAnError is the regression test for the bug this change
// fixes. A command that runs and exits non-zero has executed successfully; only its
// result is a failure. Classifying that as an execution error would discard a real exit
// status and report "the test could not run" when the test did run and found something.
func TestClassifyNonZeroExitIsNotAnError(t *testing.T) {
	codes := []int{1, 2, 126, 127, 143}

	for _, code := range codes {
		err := exec.CodeExitError{Err: errors.New("command terminated"), Code: code}

		if got := classify(err, "ns", "pod", "container"); got != nil {
			t.Errorf("classify(exit %d) = %v, want nil — a non-zero exit is a process outcome, not a failure to execute", code, got)
		}
	}
}

// TestClassifyPrefersExitStatusOverDeadline pins the ordering decision. A command can
// exit legitimately while the context deadline fires during teardown; if the deadline
// were tested first, that real exit status would be destroyed and reported as a timeout
// — which is the original defect with its sign flipped.
func TestClassifyPrefersExitStatusOverDeadline(t *testing.T) {
	// An error that is simultaneously a CodeExitError and a deadline error.
	err := &exitAndDeadlineError{code: 127}

	// Precondition: this error really does satisfy both tests, otherwise the case is vacuous.
	var codeErr exec.CodeExitError
	if !errors.As(err, &codeErr) {
		t.Fatal("precondition failed: error must satisfy errors.As for CodeExitError")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("precondition failed: error must satisfy errors.Is for DeadlineExceeded")
	}

	if got := classify(err, "ns", "pod", "container"); got != nil {
		t.Errorf("classify() = %v, want nil — a recovered exit status must win over a deadline that fired during teardown", got)
	}
}

// TestClassifyTransportFailure covers the default branch.
func TestClassifyTransportFailure(t *testing.T) {
	err := classify(errors.New("dial tcp 10.0.0.1:443: connection refused"), "ns", "pod", "container")

	if err == nil {
		t.Fatal("classify() = nil, want an error for a connection failure")
	}
	if !errors.Is(err, ErrNotExecuted) {
		t.Errorf("error %v does not match ErrNotExecuted", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Errorf("error %v matched ErrTimeout, but no deadline was involved", err)
	}
}

func TestClassifyNilIsNil(t *testing.T) {
	if got := classify(nil, "ns", "pod", "container"); got != nil {
		t.Errorf("classify(nil) = %v, want nil", got)
	}
}

// TestCheckIfFilePathExistsReportsTransportFailure covers the defect these signatures
// fix: a dead connection used to be reported as "the file does not exist", letting an
// infrastructure failure pass for a clean result.
func TestCheckIfFilePathExistsReportsTransportFailure(t *testing.T) {
	k8s := newExecTarget(t, refuseUpgrade)

	exists, err := k8s.CheckIfFilePathExists(context.Background(), "ns", "pod", "container", "/etc/shadow")

	if err == nil {
		t.Fatal("CheckIfFilePathExists() error = nil, want a transport failure — false alone would read as 'file absent'")
	}
	if exists {
		t.Error("CheckIfFilePathExists() = true, want false when the check never ran")
	}
	if !errors.Is(err, ErrNotExecuted) {
		t.Errorf("error %v does not match ErrNotExecuted", err)
	}
}

// TestCheckIfFilePathIsReadableReportsTransportFailure is the same property for the
// readability check, where a false negative is a missed finding.
func TestCheckIfFilePathIsReadableReportsTransportFailure(t *testing.T) {
	k8s := newExecTarget(t, refuseUpgrade)

	readable, err := k8s.CheckIfFilePathIsReadable(context.Background(), "ns", "pod", "container", "/etc/shadow")

	if err == nil {
		t.Fatal("CheckIfFilePathIsReadable() error = nil, want a transport failure")
	}
	if readable {
		t.Error("CheckIfFilePathIsReadable() = true, want false when the check never ran")
	}
	if !errors.Is(err, ErrNotExecuted) {
		t.Errorf("error %v does not match ErrNotExecuted", err)
	}
}

// TestReadFileReportsTransportFailure checks that a failure to reach the container is
// reported rather than retried four times, and that no partial output is returned
// alongside the error.
func TestReadFileReportsTransportFailure(t *testing.T) {
	attempts := 0
	k8s := newExecTarget(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		refuseUpgrade(w, r)
	})

	content, err := k8s.ReadFile(context.Background(), "ns", "pod", "container", "/etc/passwd")

	if err == nil {
		t.Fatal("ReadFile() error = nil, want a transport failure")
	}
	if content != "" {
		t.Errorf("ReadFile() = %q, want empty string on error", content)
	}
	if !errors.Is(err, ErrNotExecuted) {
		t.Errorf("error %v does not match ErrNotExecuted", err)
	}
	// Falling back to sed, tail and a shell loop cannot fix an unreachable container.
	if attempts > 1 {
		t.Errorf("ReadFile made %d attempts; a transport failure should stop after the first", attempts)
	}
}

// exitAndDeadlineError satisfies both errors.As for exec.CodeExitError and errors.Is
// for context.DeadlineExceeded, reproducing a command that exits just as its deadline
// expires. Constructing this by hand is the only reliable way to hit the race.
type exitAndDeadlineError struct {
	code int
}

func (e *exitAndDeadlineError) Error() string {
	return "command terminated with a deadline pending"
}

func (e *exitAndDeadlineError) Is(target error) bool {
	return target == context.DeadlineExceeded
}

func (e *exitAndDeadlineError) As(target any) bool {
	if t, ok := target.(*exec.CodeExitError); ok {
		*t = exec.CodeExitError{Err: errors.New("command terminated"), Code: e.code}
		return true
	}
	return false
}
