# k8sexec

`k8sexec` is built on the k8s.io client libraries. Its main purpose is to execute commands in
containers. `Exec` runs commands or scripts supplied through standard input (`stdin`), as arguments
(`args`), or both, and returns an
[`*execrecord.ExecutionRecord`](https://github.com/hhruszka/execrecord) holding the exit code, the
error text, and the captured stdout and stderr streams.

The module also provides functions for retrieving pods, deployments, statefulsets, daemonsets, jobs
and configmaps, which can be used to automate container enumeration.

## The execution contract

`Exec` and `ExecWithContext` return `(*execrecord.ExecutionRecord, error)`, and the two returns mean
different things:

| Result | Meaning |
|---|---|
| record, `nil` error | The command ran. `RetCode` is a real POSIX exit status. |
| `nil` record, non-nil error | Nothing ran — no exit status exists. |

**A non-zero exit code is not an error.** A command that exits `1` executed successfully; it simply
failed. That is reported as a record with `RetCode == 1` and a nil error.

An error means the command never produced a status at all: the executor could not be built, the
connection failed, the stream died, or the deadline fired. Two sentinels classify these:

```go
record, err := k8s.Exec(namespace, pod, container, args, nil, 30*time.Second)
switch {
case errors.Is(err, k8sexec.ErrTimeout):
    // Ran out of time. Nothing to report about the command itself.
case errors.Is(err, k8sexec.ErrNotExecuted):
    // Never reached the container: transport or setup failure.
case err != nil:
    return err
default:
    // record.RetCode is a genuine exit status.
}
```

`ErrTimeout` wraps `ErrNotExecuted`, so testing only for `ErrNotExecuted` catches timeouts too.
Order the cases from specific to general, as above.

This distinction matters when the result feeds a report. "The check failed" and "the check could not
be attempted" are different conclusions, and only the first is a finding.

`ExitCode` values are strictly POSIX (0–255), including the 128+n signal codes. There are no
sentinel codes for outcomes that never ran — that is what the error is for.

## Example

Embedding `lse.sh` (Linux Smart Enumeration) into the binary and running it through stdin:

```go
package simpleexec

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/hhruszka/execrecord"
	"github.com/hhruszka/k8sexec"
)

//go:embed lse.sh
var lse []byte

func test(kubeconfig string, namespace string) ([]*execrecord.ExecutionRecord, error) {
	var results []*execrecord.ExecutionRecord

	k8s, err := k8sexec.NewK8SExec(kubeconfig)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	cnt, pods, err := k8s.GetUniquePods(ctx, namespace)
	if err != nil {
		return nil, err
	}

	fmt.Printf("Found %d pods\n", cnt)

	for _, pod := range pods {
		for _, container := range pod.Spec.Containers {
			lsescript := bytes.NewBuffer(lse)

			record, err := k8s.Exec(namespace, pod.Name, container.Name,
				strings.Fields("sh -s -- -c"), lsescript, 5*time.Minute)
			if err != nil {
				// Nothing ran here, so there is no result to collect. Keep going
				// rather than abandoning the remaining containers.
				fmt.Printf("skipping %s/%s: %v\n", pod.Name, container.Name, err)
				continue
			}

			results = append(results, record)
		}
	}

	return results, nil
}
```

Use `strings.Fields(command)` so the command is passed as an argv slice:

```go
record, err := k8s.Exec(namespace, pod.Name, container.Name,
	strings.Fields(`find / -type f -perm /4000 -exec ls -l {} \; 2>/dev/null`), nil, time.Minute)
```

Note that `GetUniquePods` returns the count of *live pods*, not the length of the returned slice —
pods belonging to the same workload are collapsed, so a Deployment mid-rollout yields one entry
rather than one per ReplicaSet.

## Timeouts

`Exec` takes a `time.Duration` and builds its own context. `ExecWithContext` takes a context you
own, which is the better fit when the caller already has a deadline or a cancellation signal:

```go
record, err := k8s.ExecWithContext(ctx, namespace, pod, container, args, nil)
```

Note that `NewK8SExec` sets `config.Timeout = 0`, disabling the client-side timeout globally — this
is required for streaming exec. Every other call (`GetPods`, `GetLogs`, …) therefore runs unbounded
unless you supply a context with a deadline.

## Other execution helpers

| Function | Returns | Notes |
|---|---|---|
| `DirectExec` | `(ExitCode, error)` | Streams to your own `io.Writer`s, with optional TTY. Check the error before reading the code. |
| `ReadFile` | `(string, error)` | Tries `cat`, `sed`, `tail` and a shell read-loop in turn. Stops immediately if the container is unreachable. |
| `CheckIfFilePathExists` | `(bool, error)` | A non-nil error means the check never ran; `false` alone does not mean the file is absent. |
| `CheckIfFilePathIsReadable` | `(bool, error)` | Same contract. |
| `GetExitCode` | `(ExitCode, string, bool)` | Extracts a status from a client-go `CodeExitError`. The `bool` reports whether one was present — necessary because a command that exited 0 and an error carrying no status both yield 0. |

## Related modules

[`execrecord`](https://github.com/hhruszka/execrecord) holds `ExecutionRecord` and `ExitCode`. It
depends only on the standard library, so executors that are not Kubernetes-based can share the same
result type. `k8sexec.ExecutionStatus` and `k8sexec.ExitCode` remain as type aliases for the
`execrecord` types; new code should reference `execrecord` directly.
