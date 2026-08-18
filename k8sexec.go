package k8sexec

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hhruszka/execrecord"

	v1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"

	// these two client's plugins are not necessary for Nokia but added to have complete support
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	// oidc plugin is used in Nokia labs
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"
)

// ExecutionStatus is an alias for execrecord.ExecutionRecord. The type moved to
// github.com/hhruszka/execrecord so that executors which are not Kubernetes-based
// can share it. It is the same type, not a distinct one, so values cross the package
// boundary without conversion, and String and Raw come from there too.
//
// The record no longer carries the namespace, pod and container names: callers pass
// those as parameters when they execute, so they already hold that identity.
type ExecutionStatus = execrecord.Record

// K8SExec defines the context for modules executing commands in Kubernetes environments.
// It includes details necessary for operations, such as cluster configuration, target pod and container,
// and authentication credentials, facilitating effective interaction with Kubernetes resources.
type K8SExec struct {
	Config    *rest.Config
	Clientset kubernetes.Interface
}

// ExitCode and its constants moved to github.com/hhruszka/execrecord so that
// non-Kubernetes executors can share one definition. The alias and constants below
// keep k8sexec.ExitCode, k8sexec.Success and friends working for existing callers;
// new code should reference the execrecord package directly.

// ExitCode is an alias for execrecord.ExitCode. It is the same type, not a distinct
// one, so values cross the package boundary without conversion.
type ExitCode = execrecord.ExitCode

const (
	Success                = execrecord.Success
	GeneralError           = execrecord.GeneralError
	IncorrectUsageExitCode = execrecord.IncorrectUsageExitCode

	CommandCannotExecute       = execrecord.CommandCannotExecute
	CommandNotFound            = execrecord.CommandNotFound
	InvalidArgumentToExit      = execrecord.InvalidArgumentToExit
	ScriptTerminatedByControlC = execrecord.ScriptTerminatedByControlC
	ExitStatusOutOfRange       = execrecord.ExitStatusOutOfRange

	// Signal based exit codes (128+n)
	FatalErrorSignal1 = execrecord.FatalErrorSignal1
	// FatalErrorSignal2 is omitted as it overlaps with ScriptTerminatedByControlC
	FatalErrorSignal3  = execrecord.FatalErrorSignal3
	FatalErrorSignal4  = execrecord.FatalErrorSignal4
	FatalErrorSignal5  = execrecord.FatalErrorSignal5
	FatalErrorSignal6  = execrecord.FatalErrorSignal6
	FatalErrorSignal7  = execrecord.FatalErrorSignal7
	FatalErrorSignal8  = execrecord.FatalErrorSignal8
	FatalErrorSignal9  = execrecord.FatalErrorSignal9
	FatalErrorSignal10 = execrecord.FatalErrorSignal10
	FatalErrorSignal11 = execrecord.FatalErrorSignal11
	FatalErrorSignal12 = execrecord.FatalErrorSignal12
	FatalErrorSignal13 = execrecord.FatalErrorSignal13
	FatalErrorSignal14 = execrecord.FatalErrorSignal14
	FatalErrorSignal15 = execrecord.FatalErrorSignal15
)

// ErrNotExecuted reports that no exit status was obtained: the command never ran, or
// ran but its outcome could not be recovered. Setting up the executor failed, the
// connection failed, or the stream died mid-transfer. It is always wrapped with the
// underlying cause, so errors.Is identifies the class and errors.Unwrap reaches the
// detail.
//
// This is distinct from a command that ran and exited non-zero: that is a process
// outcome and is reported as a record with a non-zero RetCode and a nil error.
var ErrNotExecuted = errors.New("command did not produce an exit status")

// ErrTimeout reports that execution exceeded its deadline. It wraps ErrNotExecuted,
// so a caller asking only "did this produce a result?" can test ErrNotExecuted and
// catch timeouts too, while a caller that renders a distinct reason for deadlines
// tests ErrTimeout specifically.
var ErrTimeout = fmt.Errorf("%w: execution timed out", ErrNotExecuted)

// GetExitCode extracts an ExitCode and its description from the CodeExitError type
// returned by k8s.io/client-go/util/exec. It stays in this package because it is
// specific to the client-go exec backend.
//
// ok reports whether the error carried an exit status at all. When it is false the
// error came from somewhere other than a finished process and no code exists, so the
// returned ExitCode is a zero value the caller must not read. There is no longer a
// sentinel code for "no status": that distinction is what ok is for.
func GetExitCode(err error) (code ExitCode, description string, ok bool) {
	var e exec.CodeExitError
	if !errors.As(err, &e) {
		return Success, "", false
	}
	code = ExitCode(e.Code)
	if !execrecord.HasDescription(code) {
		return code, fmt.Sprintf("Exit code %d description not found!", e.Code), true
	}
	return code, code.String(), true
}

// GetExitCodeDescription returns a string description for a given exit code.
// If the code has no registered description, it returns the code's decimal value.
func GetExitCodeDescription(code ExitCode) string {
	return execrecord.Description(code)
}

// NewK8SExec creates and initializes an instance of the K8SExec type.
// It takes Kubernetes configuration information as parameters, which are required
// to access and interact with the Kubernetes cluster. This function ensures that
// the created K8SExec instance is ready to use for executing commands within Kubernetes
// pods and containers, by embedding necessary configuration details.
func NewK8SExec(kubeconfig string) (info *K8SExec, err error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	config.QPS = 100
	config.Burst = 200
	config.Timeout = 0

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &K8SExec{
		Config:    config,
		Clientset: clientset,
	}, nil
}

func (k8s *K8SExec) Clone() (*K8SExec, error) {
	copied := rest.CopyConfig(k8s.Config)
	clientset, err := kubernetes.NewForConfig(copied)
	if err != nil {
		return nil, err
	}
	return &K8SExec{
		Config:    copied,
		Clientset: clientset,
	}, nil
}

// GetClientset returns the Kubernetes clientset associated with the K8SExec instance.
func (k8s *K8SExec) GetClientset() kubernetes.Interface {
	return k8s.Clientset
}

// GetConfig returns the Kubernetes REST configuration associated with the K8SExec instance.
func (k8s *K8SExec) GetConfig() *rest.Config {
	return k8s.Config
}

// GetAllNamespaces retrieves a list of all namespace names in the Kubernetes cluster.
func (k8s *K8SExec) GetAllNamespaces(ctx context.Context) ([]string, error) {
	list, err := k8s.Clientset.CoreV1().Namespaces().List(ctx, metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var namespaces []string
	for _, ns := range list.Items {
		namespaces = append(namespaces, ns.GetName())
	}
	return namespaces, nil
}

// GetJobs retrieves all Jobs within the namespace specified by the 'k8s' context.
// This function utilizes the Kubernetes client-go library to fetch a list of Jobs
// from the specified namespace, facilitating the management and interaction with
// Kubernetes resources. It returns a list of Jobs and any error encountered during
// the retrieval process.
func (k8s *K8SExec) GetJobs(ctx context.Context, namespace string, options metaV1.ListOptions) ([]batchv1.Job, error) {
	// Retrieve jobs in the "default" namespace
	jobs, err := k8s.Clientset.BatchV1().Jobs(namespace).List(ctx, options)
	if err != nil {
		return nil, err
	}
	return jobs.Items, nil
}

// GetPod retrieves a Pod based on its name within the specified namespace.
// The namespace is provided by the 'k8s' context. This function simplifies the process
// of locating a specific Pod within a namespace, leveraging the Kubernetes client-go
// library to interact with the Kubernetes API. It returns the found Pod and any error
// encountered during the retrieval process.
func (k8s *K8SExec) GetPod(ctx context.Context, namespace string, podName string) (*coreV1.Pod, error) {
	pod, err := k8s.Clientset.CoreV1().Pods(namespace).Get(ctx, podName, metaV1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return pod, nil
}

// GetPodContainers retrieves a list of containers associated with a given Pod.
// It leverages the Kubernetes client-go library to query the Kubernetes API for Pod containers,
// facilitating the retrieval of container information for a given Pod.
func (k8s *K8SExec) GetPodContainers(ctx context.Context, namespace string, podName string) ([]coreV1.Container, error) {
	pod, err := k8s.GetPod(ctx, namespace, podName)
	if err != nil {
		return nil, err
	}
	return pod.Spec.Containers, nil
}

// GetPodContainer retrieves a specific container associated with a given Pod.
// It leverages the Kubernetes client-go library to query the Kubernetes API for Pod containers,
// facilitating the retrieval of container information for a given Pod.
func (k8s *K8SExec) GetPodContainer(ctx context.Context, namespace string, podName string, containerName string) (*coreV1.Container, error) {
	pod, err := k8s.GetPod(ctx, namespace, podName)
	if err != nil {
		return nil, err
	}
	for i, _ := range pod.Spec.Containers {
		container := pod.Spec.Containers[i]
		if container.Name == containerName {
			return &container, nil
		}
	}
	return nil, fmt.Errorf("container %s not found in pod %s", containerName, podName)
}

// GetPodDefaultContainer retrieves the default container associated with a given Pod.
// It leverages the Kubernetes client-go library to query the Kubernetes API for Pod containers,
// facilitating the retrieval of container information for a given Pod.
func (k8s *K8SExec) GetPodDefaultContainer(ctx context.Context, namespace string, podName string) (*coreV1.Container, error) {
	pod, err := k8s.GetPod(ctx, namespace, podName)
	if err != nil {
		return nil, err
	}

	// Check for the specific annotation
	if defaultName, ok := pod.ObjectMeta.Annotations["kubectl.kubernetes.io/default-container"]; ok {
		for i, _ := range pod.Spec.Containers {
			container := pod.Spec.Containers[i]
			if container.Name == defaultName {
				return &container, nil
			}
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return &pod.Spec.Containers[0], nil
	}
	return nil, fmt.Errorf("no default container found")
}

// GetPods retrieves all Pods within the namespace specified by the 'k8s' context.
// This function utilizes the Kubernetes client-go library to fetch a list of Pods
// from the specified namespace, facilitating the management and interaction with
// Kubernetes resources. It returns a list of Pods and any error encountered during
// the retrieval process.
func (k8s *K8SExec) GetPods(ctx context.Context, namespace string, options metaV1.ListOptions) ([]coreV1.Pod, error) {
	var pods *coreV1.PodList
	pods, err := k8s.Clientset.CoreV1().Pods(namespace).List(ctx, options)
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// GetDeployments retrieves all Deployments within the namespace specified in the 'k8s' context.
// It leverages the Kubernetes client-go library to query the Kubernetes API for Deployments,
// aiming to streamline the process of managing Kubernetes resources.
// This function returns an array of Deployments along with any error encountered during the query,
// thus enabling comprehensive oversight of Deployment resources within the designated namespace.
func (k8s *K8SExec) GetDeployments(ctx context.Context, namespace string) (*v1.DeploymentList, error) {
	var deployments *v1.DeploymentList
	deployments, err := k8s.Clientset.AppsV1().Deployments(namespace).List(ctx, metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return deployments, nil
}

// GetStatefulSets fetches all StatefulSets within the specified namespace, as determined by the 'k8s' context.
// Utilizing the client-go library, this function communicates with the Kubernetes API to gather StatefulSets,
// facilitating detailed management and operational oversight of these specific Kubernetes resources.
// It returns a collection of StatefulSets and any errors encountered in the process, ensuring comprehensive
// access to StatefulSet configurations within the given namespace.
func (k8s *K8SExec) GetStatefulSets(ctx context.Context, namespace string) (*v1.StatefulSetList, error) {
	var statefulSets *v1.StatefulSetList
	statefulSets, err := k8s.Clientset.AppsV1().StatefulSets(namespace).List(ctx, metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return statefulSets, nil
}

// GetDaemonSets fetches all DaemonSets within the specified namespace, as determined by the 'k8s' context.
// Utilizing the client-go library, this function communicates with the Kubernetes API to gather DaemonSets,
// facilitating detailed management and operational oversight of these specific Kubernetes resources.
// It returns a collection of StatefulSets and any errors encountered in the process, ensuring comprehensive
// access to StatefulSet configurations within the given namespace.
func (k8s *K8SExec) GetDaemonSets(ctx context.Context, namespace string) (*v1.DaemonSetList, error) {
	var daemonSets *v1.DaemonSetList
	daemonSets, err := k8s.Clientset.AppsV1().DaemonSets(namespace).List(ctx, metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return daemonSets, nil
}

// GetConfigmaps fetches all ConfigMaps within the specified namespace, as determined by the 'k8s' context.
// Utilizing the client-go library, this function communicates with the Kubernetes API to gather ConfigMaps,
// returning a collection of ConfigMap resources and any errors encountered during the process.
func (k8s *K8SExec) GetConfigmaps(ctx context.Context, namespace string) (*coreV1.ConfigMapList, error) {
	var configMaps *coreV1.ConfigMapList
	configMaps, err := k8s.Clientset.CoreV1().ConfigMaps(namespace).List(ctx, metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return configMaps, nil
}

func score(p *coreV1.Pod) int {
	switch {
	case p.DeletionTimestamp != nil:
		return 0 // being torn down; may vanish before you reach it
	case hasRunningContainer(p):
		return 3 // inspectable
	case p.Status.Phase == coreV1.PodRunning:
		return 2 // Running but nothing live — CrashLoopBackOff
	case p.Status.Phase == coreV1.PodPending:
		return 1
	default:
		return 0
	}
}

func hasRunningContainer(p *coreV1.Pod) bool {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].State.Running != nil {
			return true
		}
	}
	return false
}

func preferredPod(pod, cur *coreV1.Pod) *coreV1.Pod {
	if sp, sc := score(pod), score(cur); sp != sc {
		if sp > sc {
			return pod
		}
		return cur
	}
	if pod.CreationTimestamp.After(cur.CreationTimestamp.Time) {
		return pod
	}
	if pod.CreationTimestamp.Equal(&cur.CreationTimestamp) && pod.Name < cur.Name {
		return pod
	}
	return cur
}

// replicaSetOwners returns a map of ReplicaSet UID to the UID of its controlling
// workload (typically a Deployment). It issues a single List call for the namespace,
// so the cost is one API request regardless of how many Pods are being grouped.
// ReplicaSets without a controller are absent from the map and are grouped by their
// own UID by the caller.
func (k8s *K8SExec) replicaSetOwners(ctx context.Context, namespace string) (map[types.UID]types.UID, error) {
	rsList, err := k8s.Clientset.AppsV1().ReplicaSets(namespace).List(ctx, metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	owners := make(map[types.UID]types.UID, len(rsList.Items))
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if owner := metaV1.GetControllerOf(rs); owner != nil {
			owners[rs.UID] = owner.UID
		}
	}
	return owners, nil
}

// rootOwner returns the UID of the workload a Pod ultimately belongs to, given a
// ReplicaSet-to-workload map as produced by replicaSetOwners. Pods controlled by a
// StatefulSet, DaemonSet or Job are already at the workload level and their controller
// UID is returned unchanged. A Pod owned by a ReplicaSet resolves to the Deployment
// controlling it, falling back to the ReplicaSet's own UID when that is not known
// (a bare ReplicaSet, or an rsOwners map that could not be built).
// The second return value is false for Pods without a controller.
func rootOwner(pod *coreV1.Pod, rsOwners map[types.UID]types.UID) (types.UID, bool) {
	owner := metaV1.GetControllerOf(pod)
	if owner == nil {
		return "", false
	}
	if owner.Kind == "ReplicaSet" {
		if workload, ok := rsOwners[owner.UID]; ok {
			return workload, true
		}
	}
	return owner.UID, true
}

// GetUniquePods retrieves one representative Pod per workload within a given namespace,
// ignoring Pods that have already terminated (Succeeded or Failed).
//
// Pods are grouped by their root workload rather than their immediate controller: a Pod
// owned by a ReplicaSet is attributed to the Deployment controlling that ReplicaSet, so a
// Deployment mid-rollout yields a single Pod instead of one per ReplicaSet. Pods owned
// directly by a StatefulSet, DaemonSet or Job are already at the workload level and are
// grouped by that owner. Pods without a controller are returned individually.
//
// Where several Pods share a workload, preferredPod picks the most inspectable one.
// It returns the total number of live Pods considered and the representative Pods,
// sorted by name.
func (k8s *K8SExec) GetUniquePods(ctx context.Context, namespace string) (int, []*coreV1.Pod, error) {

	podsList, err := k8s.Clientset.CoreV1().Pods(namespace).List(ctx, metaV1.ListOptions{FieldSelector: "status.phase!=Succeeded,status.phase!=Failed"})
	if err != nil {
		return 0, nil, err
	}

	// Resolve ReplicaSet owners so Deployment-managed Pods collapse to one entry.
	// A failure here is not fatal: grouping falls back to the immediate controller.
	rsOwners, err := k8s.replicaSetOwners(ctx, namespace)
	if err != nil {
		rsOwners = nil
	}

	var notOwnedPods []*coreV1.Pod
	var ownedPods map[types.UID]*coreV1.Pod = make(map[types.UID]*coreV1.Pod)

	for i := range podsList.Items {
		pod := &podsList.Items[i]
		key, owned := rootOwner(pod, rsOwners)
		if !owned {
			notOwnedPods = append(notOwnedPods, pod)
			continue
		}
		if cur, ok := ownedPods[key]; ok {
			preferred := preferredPod(pod, cur)
			ownedPods[key] = preferred
			continue
		}
		ownedPods[key] = pod
	}

	uniquePods := make([]*coreV1.Pod, 0, len(ownedPods))
	for _, pod := range ownedPods {
		uniquePods = append(uniquePods, pod)
	}

	uniquePods = append(uniquePods, notOwnedPods...)
	sort.Slice(uniquePods, func(i, j int) bool { return uniquePods[i].Name < uniquePods[j].Name })
	return len(podsList.Items), uniquePods, nil
}

// GetUniqueImages retrieves a comprehensive and unique list of Pods within a given namespace,
// as provided by the 'k8s' context. It targets Pods associated with Deployments, StatefulSets,
// and those directly within the namespace, ensuring no duplicates.
func (k8s *K8SExec) GetUniqueImages(ctx context.Context, namespace string) (int, []string, error) {
	var images []string
	var containersCount int

	podsList, err := k8s.Clientset.CoreV1().Pods(namespace).List(ctx, metaV1.ListOptions{})
	if err != nil {
		return 0, nil, err
	}

	for _, pod := range podsList.Items {
		containersCount += len(pod.Spec.Containers)
		for _, container := range pod.Spec.Containers {
			if slices.Contains(images, container.Image) {
				continue
			}
			images = append(images, container.Image)
		}
	}

	return containersCount, images, nil
}

// GetLogs retrieves the logs from the specified pod and container within the Kubernetes namespace of the K8SExec instance.
// It returns the size of the logs in bytes, the log data as a byte slice, and an error if the operation fails.
func (k8s *K8SExec) GetLogs(ctx context.Context, namespace string, podName string, containerName string) (int, []byte, error) {
	// Request logs
	opt := new(coreV1.PodLogOptions)
	opt.Container = containerName
	req := k8s.Clientset.CoreV1().Pods(namespace).GetLogs(podName, opt)

	// Read log stream
	logReader, err := req.Stream(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer logReader.Close()

	//runtime.ErrorHandlers = []runtime.ErrorHandler{
	//	func(ctx context.Context, err error, msg string, keysAndValues ...interface{}) {
	//		// ignore unhandled errors
	//		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.ErrUnexpectedEOF) {
	//			return
	//		}
	//
	//	},
	//}

	buf := new(bytes.Buffer)
	r := bufio.NewReader(logReader)
	for {
		data, err := r.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, nil, err
			}
			break
		}
		if _, err := buf.Write(data); err != nil {
			return 0, nil, err
		}
	}

	return buf.Len(), buf.Bytes(), nil
}

func (k8s *K8SExec) ReadFile(ctx context.Context, namespace string, podName, containerName string, filePath string) (string, error) {
	var stdout, stderr bytes.Buffer
	var err error
	var retCode ExitCode

	var attempts [][]string = [][]string{
		[]string{"cat", filePath},
		[]string{"sed", "", filePath},
		[]string{"tail", "-n", "+1", filePath},
		[]string{
			"sh", "-c",
			`while IFS= read -r line; do echo "$line"; done < "$0"`,
			filePath,
		},
	}

	for _, attempt := range attempts {
		stdout.Reset()
		stderr.Reset()
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)

		retCode, err = k8s.exec(attemptCtx, namespace, podName, containerName, attempt, nil, &stdout, &stderr, false)
		cancel()
		if retCode == Success && err == nil {
			return stdout.String(), nil
		}

		// A failure to reach the container will not be fixed by reading the file a
		// different way, so stop rather than spending three more attempts on it. Only
		// a genuine non-zero exit means "this utility could not read the file" and is
		// worth falling through to the next attempt.
		if execErr := classify(err, namespace, podName, containerName); execErr != nil {
			return "", execErr
		}
		if ctx.Err() != nil {
			break
		}
	}

	if err != nil {
		return "", fmt.Errorf("could not read %s in %s/%s: %w", filePath, podName, containerName, err)
	}

	return "", fmt.Errorf("could not read %s in %s/%s", filePath, podName, containerName)
}

// classify turns an error from exec into the package's error contract. It returns nil
// when the command ran — including when it exited non-zero, which is a process outcome
// and not an execution failure — and otherwise wraps the cause in ErrNotExecuted or
// ErrTimeout.
//
// The CodeExitError test comes first for the same reason as in ExecWithContext: a
// command can exit legitimately while the deadline fires during teardown, and a real
// exit status must not be reclassified as a timeout.
func classify(err error, namespace, podName, containerName string) error {
	if err == nil {
		return nil
	}

	var codeExitError exec.CodeExitError
	switch {
	case errors.As(err, &codeExitError):
		// A non-zero exit code is a process outcome, not an execution failure.
		return nil
	case net.IsTimeout(err) || errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %s/%s in %s: %w", ErrTimeout, podName, containerName, namespace, err)
	default:
		return fmt.Errorf("%w: %s/%s in %s: %w", ErrNotExecuted, podName, containerName, namespace, err)
	}
}

// CheckIfFilePathIsReadable determines if a file at the given path in a specified container and pod is readable.
//
// A non-nil error means the check could not be performed at all, which is not the same
// as the file being unreadable: reporting a dead connection as "not readable" would let
// an infrastructure failure masquerade as a security finding. Callers must distinguish
// the two rather than treating false as conclusive.
func (k8s *K8SExec) CheckIfFilePathIsReadable(ctx context.Context, namespace string, podName, containerName string, filePath string) (bool, error) {
	var stdout, stderr bytes.Buffer

	attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)

	retCode, err := k8s.exec(attemptCtx, namespace, podName, containerName, []string{"stat", "-c", "%a", filePath}, nil, &stdout, &stderr, false)

	cancel()

	if err := classify(err, namespace, podName, containerName); err != nil {
		return false, err
	}

	// stat failed, let's try 'test -r'
	if retCode != Success {
		stdout.Reset()
		stderr.Reset()
		attemptCtx, cancel = context.WithTimeout(ctx, 5*time.Second)

		retCode, err = k8s.exec(attemptCtx, namespace, podName, containerName, []string{"sh", "-c", `test -r "$0"`, filePath}, nil, &stdout, &stderr, false)

		cancel()

		if err := classify(err, namespace, podName, containerName); err != nil {
			return false, err
		}

		return retCode == Success, nil
	}

	// stat was successful so let's analyze permissions
	permStr := strings.TrimSpace(stdout.String())
	if len(permStr) >= 3 {
		if len(permStr) == 4 {
			permStr = permStr[1:]
		}

		ownerPerm := int(permStr[0] - '0')
		groupPerm := int(permStr[1] - '0')
		othersPerm := int(permStr[2] - '0')
		const readBit = 4

		return ownerPerm&readBit != 0 || groupPerm&readBit != 0 || othersPerm&readBit != 0, nil
	}

	return false, nil
}

// CheckIfFilePathExists reports whether a file exists at the given path in a container.
//
// As with CheckIfFilePathIsReadable, a non-nil error means the check never happened and
// false says nothing about the file.
func (k8s *K8SExec) CheckIfFilePathExists(ctx context.Context, namespace string, podName, containerName string, filePath string) (bool, error) {
	var stdout, stderr bytes.Buffer

	attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)

	retCode, err := k8s.exec(attemptCtx, namespace, podName, containerName, []string{"stat", filePath}, nil, &stdout, &stderr, false)

	cancel()

	if err := classify(err, namespace, podName, containerName); err != nil {
		return false, err
	}

	if retCode != Success {
		stdout.Reset()
		stderr.Reset()
		attemptCtx, cancel = context.WithTimeout(ctx, 5*time.Second)

		retCode, err = k8s.exec(attemptCtx, namespace, podName, containerName, []string{"sh", "-c", `test -f "$0"`, filePath}, nil, &stdout, &stderr, false)

		cancel()

		if err := classify(err, namespace, podName, containerName); err != nil {
			return false, err
		}
	}

	return retCode == Success, nil
}

// exec executes a command provided via standard input ('stdin'), command-line arguments ('cmd'),
// or both, offering a versatile interface for command execution. Upon completion, it returns a POSIX
// execution code to indicate the success or failure of the operation, alongside any error encountered
// during execution for detailed diagnostics. Additionally, the function captures and returns both
// the standard output ('stdout') and standard error ('stderr') streams, providing details of the command's execution.
//
// The returned ExitCode is meaningful only when the error is nil or is an exec.CodeExitError.
// For any other error no exit status was obtained, and the code is a zero value the caller
// must not interpret — check the error first.
func (k8s *K8SExec) exec(ctx context.Context, namespace string, podName string, containerName string, cmd []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, tty bool) (ExitCode, error) {
	req := k8s.Clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&coreV1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
			TTY:       tty,
		}, scheme.ParameterCodec)

	spdyExecutor, err := remotecommand.NewSPDYExecutor(k8s.Config, "POST", req.URL())
	if err != nil {
		return Success, fmt.Errorf("failed to create SPDY executor: %w", err)
	}

	// WebSockets strictly use GET for the HTTP upgrade handshake
	wsExecutor, err := remotecommand.NewWebSocketExecutor(k8s.Config, "GET", req.URL().String())
	if err != nil {
		return Success, fmt.Errorf("failed to create WebSocket executor: %w", err)
	}

	// This attempts WebSockets first. If the server rejects the upgrade (e.g., an older
	// Kubernetes version), it evaluates the fallback condition and drops down to SPDY.
	executor, err := remotecommand.NewFallbackExecutor(wsExecutor, spdyExecutor, func(err error) bool {
		// httpstream.IsUpgradeFailure checks if the error indicates the server doesn't support the protocol
		return httpstream.IsUpgradeFailure(err)
	})
	if err != nil {
		return Success, fmt.Errorf("failed to create fallback executor: %w", err)
	}

	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    tty,
	})

	codeExitError := exec.CodeExitError{}

	switch {
	case err == nil:
		return Success, nil
	case errors.As(err, &codeExitError):
		code := codeExitError.ExitStatus()
		return ExitCode(code), codeExitError
	default:
		// No exit status was recovered. The code returned here carries no meaning;
		// the error is the whole result.
		return Success, err
	}
}

// DirectExec executes a command inside a specified container of a pod, with I/O streams and TTY access if enabled.
func (k8s *K8SExec) DirectExec(ctx context.Context, namespace string, podName string, containerName string, cmd []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, tty bool) (ExitCode, error) {
	return k8s.exec(ctx, namespace, podName, containerName, cmd, stdin, stdout, stderr, tty)
}

// NewExecutionStatus initializes a new instance of the ExecutionStatus type, providing a method
// to encapsulate the outcome of a command's execution within a structured format.
// This function serves as a constructor, setting up an ExecutionStatus instance.
//
// It delegates to execrecord.NewRecord; prefer calling that directly in new code.
func NewExecutionStatus(retCode ExitCode, error string, stdout string, stderr string, execTime time.Time) *ExecutionStatus {
	return execrecord.NewRecord(retCode, error, stdout, stderr, execTime)
}

// Exec executes a command provided through standard input ('stdin') or as arguments ('args'),
// or a combination of both, bounded by the given timeout.
//
// See ExecWithContext for the contract; this differs only in building the context.
func (k8s *K8SExec) Exec(namespace string, podName string, containerName string, args []string, stdin io.Reader, timeout time.Duration) (*execrecord.Record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return k8s.ExecWithContext(ctx, namespace, podName, containerName, args, stdin)
}

// ExecWithContext executes a command provided through standard input ('stdin') or as
// arguments ('args'), or a combination of both, governed by the supplied context.
//
// It returns a record only when the command actually ran. RetCode is then a real POSIX
// exit status, and a non-zero one is reported as a record with a nil error: a command
// that exits 1 has executed successfully, it has merely failed.
//
// When no exit status could be obtained the record is nil and the error wraps
// ErrNotExecuted — or ErrTimeout, which itself wraps ErrNotExecuted, when the deadline
// fired. A nil record means nothing ran, which is a different fact from a command that
// ran and failed, and callers should render it differently.
func (k8s *K8SExec) ExecWithContext(ctx context.Context, namespace string, podName string, containerName string, args []string, stdin io.Reader) (*execrecord.Record, error) {
	var stdout, stderr bytes.Buffer
	var errMessage string

	execTime := time.Now()
	retCode, err := k8s.exec(ctx, namespace, podName, containerName, args, stdin, &stdout, &stderr, false)

	if execErr := classify(err, namespace, podName, containerName); execErr != nil {
		return nil, execErr
	}

	return execrecord.NewRecord(retCode, errMessage, stdout.String(), stderr.String(), execTime), nil
}
