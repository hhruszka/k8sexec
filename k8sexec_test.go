package k8sexec

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hhruszka/execrecord"
	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/exec"
)

// controllerRef builds an OwnerReference marked as the controlling owner.
func controllerRef(kind, name string, uid types.UID) metaV1.OwnerReference {
	controller := true
	return metaV1.OwnerReference{Kind: kind, Name: name, UID: uid, Controller: &controller}
}

func podWithOwners(refs ...metaV1.OwnerReference) *coreV1.Pod {
	return &coreV1.Pod{ObjectMeta: metaV1.ObjectMeta{OwnerReferences: refs}}
}

// TestRootOwner covers the workload-grouping rules: Deployment-managed Pods must collapse
// onto the Deployment rather than their individual ReplicaSets, while workload-level
// controllers pass through unchanged.
func TestRootOwner(t *testing.T) {
	// Two ReplicaSets belonging to the same Deployment, as seen mid-rollout.
	rsOwners := map[types.UID]types.UID{
		"rs-old": "deploy-1",
		"rs-new": "deploy-1",
	}

	tests := []struct {
		name      string
		pod       *coreV1.Pod
		wantUID   types.UID
		wantOwned bool
	}{
		{
			name:      "deployment rollout, old ReplicaSet",
			pod:       podWithOwners(controllerRef("ReplicaSet", "app-5f7b", "rs-old")),
			wantUID:   "deploy-1",
			wantOwned: true,
		},
		{
			name:      "deployment rollout, new ReplicaSet",
			pod:       podWithOwners(controllerRef("ReplicaSet", "app-9c2d", "rs-new")),
			wantUID:   "deploy-1",
			wantOwned: true,
		},
		{
			name:      "bare ReplicaSet falls back to its own UID",
			pod:       podWithOwners(controllerRef("ReplicaSet", "standalone", "rs-bare")),
			wantUID:   "rs-bare",
			wantOwned: true,
		},
		{
			name:      "StatefulSet is already workload level",
			pod:       podWithOwners(controllerRef("StatefulSet", "db", "sts-1")),
			wantUID:   "sts-1",
			wantOwned: true,
		},
		{
			name:      "DaemonSet is already workload level",
			pod:       podWithOwners(controllerRef("DaemonSet", "agent", "ds-1")),
			wantUID:   "ds-1",
			wantOwned: true,
		},
		{
			name:      "Job is already workload level",
			pod:       podWithOwners(controllerRef("Job", "migrate", "job-1")),
			wantUID:   "job-1",
			wantOwned: true,
		},
		{
			name:      "Pod with no owner is not grouped",
			pod:       podWithOwners(),
			wantUID:   "",
			wantOwned: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUID, gotOwned := rootOwner(tt.pod, rsOwners)
			if gotUID != tt.wantUID || gotOwned != tt.wantOwned {
				t.Errorf("rootOwner() = (%q, %v), want (%q, %v)", gotUID, gotOwned, tt.wantUID, tt.wantOwned)
			}
		})
	}
}

// TestRootOwnerNilMap guards the degraded path: when replicaSetOwners fails, GetUniquePods
// passes a nil map and grouping must fall back to the immediate controller without panicking.
func TestRootOwnerNilMap(t *testing.T) {
	pod := podWithOwners(controllerRef("ReplicaSet", "app-5f7b", "rs-old"))

	gotUID, gotOwned := rootOwner(pod, nil)
	if gotUID != "rs-old" || !gotOwned {
		t.Errorf("rootOwner() with nil map = (%q, %v), want (\"rs-old\", true)", gotUID, gotOwned)
	}
}

// TestRootOwnerIgnoresNonControllerRef ensures an ownerReference that is not the controller
// (Controller: false) does not group the Pod — matching metaV1.GetControllerOf semantics.
func TestRootOwnerIgnoresNonControllerRef(t *testing.T) {
	notController := false
	ref := metaV1.OwnerReference{Kind: "ReplicaSet", Name: "app", UID: "rs-old", Controller: &notController}

	if gotUID, gotOwned := rootOwner(podWithOwners(ref), map[types.UID]types.UID{"rs-old": "deploy-1"}); gotOwned {
		t.Errorf("rootOwner() = (%q, %v), want owned=false for a non-controller ownerReference", gotUID, gotOwned)
	}
}

// runningPod builds a Pod whose container status reports a running state.
func runningPod(name string, created time.Time) *coreV1.Pod {
	return &coreV1.Pod{
		ObjectMeta: metaV1.ObjectMeta{Name: name, CreationTimestamp: metaV1.NewTime(created)},
		Status: coreV1.PodStatus{
			Phase: coreV1.PodRunning,
			ContainerStatuses: []coreV1.ContainerStatus{
				{State: coreV1.ContainerState{Running: &coreV1.ContainerStateRunning{}}},
			},
		},
	}
}

// phasePod builds a Pod in the given phase with no running containers.
func phasePod(name string, phase coreV1.PodPhase, created time.Time) *coreV1.Pod {
	return &coreV1.Pod{
		ObjectMeta: metaV1.ObjectMeta{Name: name, CreationTimestamp: metaV1.NewTime(created)},
		Status:     coreV1.PodStatus{Phase: phase},
	}
}

func TestScore(t *testing.T) {
	now := time.Now()

	terminating := runningPod("terminating", now)
	deleted := metaV1.NewTime(now)
	terminating.DeletionTimestamp = &deleted

	tests := []struct {
		name string
		pod  *coreV1.Pod
		want int
	}{
		{"running container is most inspectable", runningPod("live", now), 3},
		{"Running phase without a live container", phasePod("crashloop", coreV1.PodRunning, now), 2},
		{"Pending", phasePod("pending", coreV1.PodPending, now), 1},
		{"Unknown phase", phasePod("unknown", coreV1.PodUnknown, now), 0},
		{"terminating outranked despite running container", terminating, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := score(tt.pod); got != tt.want {
				t.Errorf("score() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestPreferredPod covers representative selection: score dominates, then newer
// CreationTimestamp, then lexically smaller name as a deterministic tiebreak.
func TestPreferredPod(t *testing.T) {
	now := time.Now()
	older := now.Add(-time.Hour)

	tests := []struct {
		name     string
		pod      *coreV1.Pod
		cur      *coreV1.Pod
		wantName string
	}{
		{
			name:     "higher score wins over newer timestamp",
			pod:      runningPod("running-old", older),
			cur:      phasePod("pending-new", coreV1.PodPending, now),
			wantName: "running-old",
		},
		{
			name:     "incumbent kept when it scores higher",
			pod:      phasePod("pending-new", coreV1.PodPending, now),
			cur:      runningPod("running-old", older),
			wantName: "running-old",
		},
		{
			name:     "equal score prefers the newer Pod",
			pod:      runningPod("newer", now),
			cur:      runningPod("older", older),
			wantName: "newer",
		},
		{
			name:     "equal score and age prefers the lexically smaller name",
			pod:      runningPod("aaa", now),
			cur:      runningPod("zzz", now),
			wantName: "aaa",
		},
		{
			name:     "equal score and age keeps incumbent when its name sorts first",
			pod:      runningPod("zzz", now),
			cur:      runningPod("aaa", now),
			wantName: "aaa",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := preferredPod(tt.pod, tt.cur); got.Name != tt.wantName {
				t.Errorf("preferredPod() = %q, want %q", got.Name, tt.wantName)
			}
		})
	}
}

// TestExecutionStatusAliasIsSharedType pins the alias contract, as
// TestExitCodeAliasIsSharedType does for ExitCode: k8sexec.ExecutionStatus must be
// the *same* type as execrecord.ExecutionRecord. These assignments in both directions
// fail to compile if the alias becomes a defined type.
func TestExecutionStatusAliasIsSharedType(t *testing.T) {
	now := time.Now()

	var fromLocal *ExecutionStatus = execrecord.New(Success, "", "out", "", now)
	var fromShared *execrecord.ExecutionRecord = NewExecutionStatus(Success, "", "out", "", now)

	if fromLocal.RetCode != fromShared.RetCode {
		t.Errorf("RetCode mismatch: %v != %v", fromLocal.RetCode, fromShared.RetCode)
	}
}

// TestNewExecutionStatusDelegates checks the shim forwards its arguments in the right
// order. The line-splitting contract itself is covered in the execrecord package.
func TestNewExecutionStatusDelegates(t *testing.T) {
	now := time.Now()
	got := NewExecutionStatus(CommandNotFound, "boom", "out line\n", "err line\n", now)

	if got.RetCode != CommandNotFound {
		t.Errorf("RetCode = %v, want %v", got.RetCode, CommandNotFound)
	}
	if len(got.Error) != 1 || got.Error[0] != "boom" {
		t.Errorf("Error = %#v, want [\"boom\"]", got.Error)
	}
	if len(got.Stdout) != 1 || got.Stdout[0] != "out line" {
		t.Errorf("Stdout = %#v, want [\"out line\"]", got.Stdout)
	}
	if len(got.Stderr) != 1 || got.Stderr[0] != "err line" {
		t.Errorf("Stderr = %#v, want [\"err line\"]", got.Stderr)
	}
	if !got.ExecTime.Equal(now) {
		t.Errorf("ExecTime = %v, want %v", got.ExecTime, now)
	}
}

// TestExitCodeAliasIsSharedType pins the alias contract: k8sexec.ExitCode must be the
// *same* type as execrecord.ExitCode, not a defined type wrapping it. If someone changes
// `type ExitCode = execrecord.ExitCode` to `type ExitCode execrecord.ExitCode`, existing
// callers keep compiling but silently need conversions at the boundary. These
// assignments in both directions fail to compile under that change.
func TestExitCodeAliasIsSharedType(t *testing.T) {
	var fromShared execrecord.ExitCode = Success
	var fromLocal ExitCode = execrecord.Success

	if fromShared != fromLocal {
		t.Errorf("alias mismatch: %v != %v", fromShared, fromLocal)
	}

	// The re-exported constants must carry the same values as their originals.
	pairs := []struct {
		name   string
		local  ExitCode
		shared execrecord.ExitCode
	}{
		{"Success", Success, execrecord.Success},
		{"GeneralError", GeneralError, execrecord.GeneralError},
		{"CommandNotFound", CommandNotFound, execrecord.CommandNotFound},
		{"CommandCannotExecute", CommandCannotExecute, execrecord.CommandCannotExecute},
		{"FatalErrorSignal15", FatalErrorSignal15, execrecord.FatalErrorSignal15},
	}

	for _, p := range pairs {
		if p.local != p.shared {
			t.Errorf("%s: k8sexec = %d, execrecord = %d", p.name, int(p.local), int(p.shared))
		}
	}
}

// TestGetExitCode covers extraction of an exit status from a client-go CodeExitError,
// including the non-CodeExitError path and codes with no registered description.
func TestGetExitCode(t *testing.T) {
	tests := []struct {
		name            string
		err             error
		wantCode        ExitCode
		wantDescription string
		wantOK          bool
	}{
		{
			name:            "known code carries its description",
			err:             exec.CodeExitError{Err: errors.New("command terminated"), Code: 127},
			wantCode:        CommandNotFound,
			wantDescription: "Command not found",
			wantOK:          true,
		},
		{
			name:            "success",
			err:             exec.CodeExitError{Err: errors.New("ok"), Code: 0},
			wantCode:        Success,
			wantDescription: "Success",
			wantOK:          true,
		},
		{
			name:            "unmapped code reports the description as missing",
			err:             exec.CodeExitError{Err: errors.New("odd"), Code: 77},
			wantCode:        ExitCode(77),
			wantDescription: "Exit code 77 description not found!",
			wantOK:          true,
		},
		{
			// The distinguishing signal is ok, not a sentinel code. Success here is a
			// zero value the caller must not read, which is the whole point of ok.
			name:            "non-CodeExitError reports no exit status",
			err:             errors.New("dial tcp: connection refused"),
			wantCode:        Success,
			wantDescription: "",
			wantOK:          false,
		},
		{
			name:            "wrapped CodeExitError is unwrapped",
			err:             fmt.Errorf("exec failed: %w", exec.CodeExitError{Err: errors.New("boom"), Code: 126}),
			wantCode:        CommandCannotExecute,
			wantDescription: "Command cannot execute",
			wantOK:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCode, gotDescription, gotOK := GetExitCode(tt.err)
			if gotCode != tt.wantCode || gotDescription != tt.wantDescription || gotOK != tt.wantOK {
				t.Errorf("GetExitCode() = (%d, %q, %t), want (%d, %q, %t)",
					int(gotCode), gotDescription, gotOK, int(tt.wantCode), tt.wantDescription, tt.wantOK)
			}
		})
	}
}

// TestGetExitCodeDistinguishesZeroFromNoStatus pins the one case ok exists to resolve:
// a command that genuinely exited 0 and an error carrying no status both yield a code
// of 0, and only ok tells them apart.
func TestGetExitCodeDistinguishesZeroFromNoStatus(t *testing.T) {
	ranAndSucceeded, _, okRan := GetExitCode(exec.CodeExitError{Err: errors.New("ok"), Code: 0})
	neverRan, _, okNever := GetExitCode(errors.New("connection refused"))

	if ranAndSucceeded != neverRan {
		t.Fatalf("precondition failed: expected both codes to be 0, got %d and %d", int(ranAndSucceeded), int(neverRan))
	}
	if !okRan {
		t.Error("a command that exited 0 must report ok=true")
	}
	if okNever {
		t.Error("an error with no exit status must report ok=false")
	}
}

func TestGetExitCodeDescription(t *testing.T) {
	tests := []struct {
		code ExitCode
		want string
	}{
		{Success, "Success"},
		{CommandNotFound, "Command not found"},
		{FatalErrorSignal15, "Fatal error signal 15 (SIGTERM)"},
		{ExitCode(77), "77"},
	}

	for _, tt := range tests {
		if got := GetExitCodeDescription(tt.code); got != tt.want {
			t.Errorf("GetExitCodeDescription(%d) = %q, want %q", int(tt.code), got, tt.want)
		}
	}
}

func TestExitCodeString(t *testing.T) {
	tests := []struct {
		code ExitCode
		want string
	}{
		{Success, "Success"},
		{CommandNotFound, "Command not found"},
		{CommandCannotExecute, "Command cannot execute"},
		{ExitCode(77), "77"}, // unmapped codes render as their number
	}

	for _, tt := range tests {
		if got := tt.code.String(); got != tt.want {
			t.Errorf("ExitCode(%d).String() = %q, want %q", int(tt.code), got, tt.want)
		}
	}
}
