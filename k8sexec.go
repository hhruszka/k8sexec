package k8sexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	v1 "k8s.io/api/apps/v1"
	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	exec2 "k8s.io/client-go/util/exec"
	"slices"
	"strings"
	"time"
)

// ExecutionStatus encapsulates the result and details of executing a command within a specific container.
// It includes both identification and outcome information. The container is specified by its name and
// the associated pod's name, provided in the Container and Pod fields, respectively.
// The execution outcome is detailed as follows:
// - RetCode: The exit code of the command executed within the container. A zero value typically indicates success.
// - Error: A string representation of any error that occurred during command execution, as reported by the Kubernetes API.
// - Stdout: The standard output generated by the command.
// - Stderr: The standard error output generated by the command, if any.
type ExecutionStatus struct {
	Pod       string   `json:"Pod"`
	Container string   `json:"Container"`
	RetCode   ExitCode `json:"RetCode"`
	Error     []string `json:"Error"`
	Stdout    []string `json:"Stdout"`
	Stderr    []string `json:"Stderr"`
}

// K8SExec defines the context for modules executing commands in Kubernetes environments.
// It includes details necessary for operations, such as cluster configuration, target pod and container,
// and authentication credentials, facilitating effective interaction with Kubernetes resources.
type K8SExec struct {
	Config    *rest.Config
	Clientset *kubernetes.Clientset
	Namespace string
}

// ExitCode is an enumeration of possible exit codes with descriptive names.
// It provides a more idiomatic way to refer to exit codes within the Go application.
type ExitCode int

const (
	ExecutionTimeOut ExitCode = iota - 2
	InternalAppError
	Success
	GeneralError
	IncorrectUsage
	CommandCannotExecute  = 126
	CommandNotFound       = 127
	InvalidArgumentToExit = 128
	// Skips to specific values after the iota increment
	ScriptTerminatedByControlC ExitCode = 130
	ExitStatusOutOfRange       ExitCode = 255
	// Signal based exit codes (128+n)
	FatalErrorSignal1 ExitCode = 129
	// FatalErrorSignal2 is omitted as it overlaps with ScriptTerminatedByControlC
	FatalErrorSignal3  ExitCode = 131
	FatalErrorSignal4  ExitCode = 132
	FatalErrorSignal5  ExitCode = 133
	FatalErrorSignal6  ExitCode = 134
	FatalErrorSignal7  ExitCode = 135
	FatalErrorSignal8  ExitCode = 136
	FatalErrorSignal9  ExitCode = 137
	FatalErrorSignal10 ExitCode = 138
	FatalErrorSignal11 ExitCode = 139
	FatalErrorSignal12 ExitCode = 140
	FatalErrorSignal13 ExitCode = 141
	FatalErrorSignal14 ExitCode = 142
	FatalErrorSignal15 ExitCode = 143
)

// exitCodeDescriptions maps possible exit codes with descriptive names.
var exitCodeDescriptions map[ExitCode]string = map[ExitCode]string{
	-1:  "Internal app error",
	0:   "Success",
	1:   "General error, unspecified error",
	2:   "Incorrect usage or syntax of the command",
	126: "Command cannot execute",
	127: "Command not found",
	128: "Invalid argument to exit",
	130: "Script terminated by Control-C (SIGINT)",
	255: "Exit status out of range",
	// Signal based exit codes (128+n)
	129: "Fatal error signal 1 (SIGHUP)",
	//130: "Fatal error signal 2 (SIGINT)",
	131: "Fatal error signal 3 (SIGQUIT)",
	132: "Fatal error signal 4 (SIGILL)",
	133: "Fatal error signal 5 (SIGTRAP)",
	134: "Fatal error signal 6 (SIGABRT/SIGIOT)",
	135: "Fatal error signal 7 (SIGBUS)",
	136: "Fatal error signal 8 (SIGFPE)",
	137: "Fatal error signal 9 (SIGKILL)",
	138: "Fatal error signal 10 (SIGUSR1)",
	139: "Fatal error signal 11 (SIGSEGV)",
	140: "Fatal error signal 12 (SIGUSR2)",
	141: "Fatal error signal 13 (SIGPIPE)",
	142: "Fatal error signal 14 (SIGALRM)",
	143: "Fatal error signal 15 (SIGTERM)",
	// Add more signal based codes as needed
}

// GetExitCode returns an ExitCode retrieved from CodeExitError type returned by k8s.io/client-go/util/exec and
// a corresponding description from exitCodeDescriptions map.
func GetExitCode(err error) (ExitCode, string) {
	var e exec2.CodeExitError
	if !errors.As(err, &e) {
		return InternalAppError, ""
	}
	if _, ok := exitCodeDescriptions[ExitCode(e.Code)]; !ok {
		return ExitCode(e.Code), fmt.Sprintf("Exit code %d description not found!", e.Code)
	}
	return ExitCode(e.Code), exitCodeDescriptions[ExitCode(e.Code)]
}

// GetExitCodeDescription returns a string description for a given exit code.
// It looks up the code in the predefined exitCodeDescriptions map. If the code is found,
// it returns the corresponding description. If not, it returns "Unknown exit code".
func GetExitCodeDescription(code ExitCode) string {
	if _, ok := exitCodeDescriptions[code]; !ok {
		return ""
	}
	return exitCodeDescriptions[code]
}

// NewK8SExec creates and initializes an instance of the K8SExec type.
// It takes Kubernetes configuration information as parameters, which are required
// to access and interact with the Kubernetes cluster. This function ensures that
// the created K8SExec instance is ready to use for executing commands within Kubernetes
// pods and containers, by embedding necessary configuration details.
func NewK8SExec(kubeconfig string, namespace string) (info *K8SExec, err error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return &K8SExec{Config: config, Clientset: clientset, Namespace: namespace}, nil
}

// GetPod retrieves a Pod based on its name within the specified namespace.
// The namespace is provided by the 'k8s' context. This function simplifies the process
// of locating a specific Pod within a namespace, leveraging the Kubernetes client-go
// library to interact with the Kubernetes API. It returns the found Pod and any error
// encountered during the retrieval process.
func (k8s *K8SExec) GetPod(podName string, options metaV1.GetOptions) (*coreV1.Pod, error) {
	pod, err := k8s.Clientset.CoreV1().Pods(k8s.Namespace).Get(context.TODO(), podName, metaV1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return pod, nil
}

// GetPods retrieves all Pods within the namespace specified by the 'k8s' context.
// This function utilizes the Kubernetes client-go library to fetch a list of Pods
// from the specified namespace, facilitating the management and interaction with
// Kubernetes resources. It returns a list of Pods and any error encountered during
// the retrieval process.
func (k8s *K8SExec) GetPods(options metaV1.ListOptions) ([]coreV1.Pod, error) {
	var pods *coreV1.PodList
	pods, err := k8s.Clientset.CoreV1().Pods(k8s.Namespace).List(context.TODO(), options)
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
func (k8s *K8SExec) GetDeployments() (*v1.DeploymentList, error) {
	var deployments *v1.DeploymentList
	deployments, err := k8s.Clientset.AppsV1().Deployments(k8s.Namespace).List(context.TODO(), metaV1.ListOptions{})
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
func (k8s *K8SExec) GetStatefulSets() (*v1.StatefulSetList, error) {
	var statefulSets *v1.StatefulSetList
	statefulSets, err := k8s.Clientset.AppsV1().StatefulSets(k8s.Namespace).List(context.TODO(), metaV1.ListOptions{})
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
func (k8s *K8SExec) GetDaemonSets() (*v1.DaemonSetList, error) {
	var daemonSets *v1.DaemonSetList
	daemonSets, err := k8s.Clientset.AppsV1().DaemonSets(k8s.Namespace).List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return daemonSets, nil
}

// mapToLabelSelector takes a map containing key-value pairs and converts it into a Kubernetes label selector
// string format. This utility function is essential for crafting label selectors used in Kubernetes API queries,
// allowing for the filtering of resources based on specified labels. The resulting string is a concatenation of
// the map's key-value pairs, formatted as 'key=value', and joined by commas for multiple pairs.
// This conversion facilitates the dynamic selection of Kubernetes resources based on labels, enhancing
// the flexibility and precision of resource queries within Kubernetes operations.
func mapToLabelSelector(labels map[string]string) string {
	var selectorParts []string
	for key, value := range labels {
		selectorParts = append(selectorParts, fmt.Sprintf("%s=%s", key, value))
	}
	return strings.Join(selectorParts, ",")
}

// GetPods retrieves a comprehensive and unique list of Pods within a given namespace,
// as provided by the 'k8s' context. It targets Pods associated with Deployments, StatefulSets,
// and those directly within the namespace, ensuring no duplicates.
func (k8s *K8SExec) GetUniquePods() (int, []coreV1.Pod, error) {
	var uniquePods []coreV1.Pod

	var deploymentPods map[string]int = make(map[string]int)
	deployments, err := k8s.GetDeployments()
	if err != nil {
		return 0, nil, err
	}

	for _, deployment := range deployments.Items {
		// to find all pods that are part of a given deployment we need to use deployment.Spec.Selector.MatchLabels
		// from the deployment. This is essential.
		options := metaV1.ListOptions{LabelSelector: mapToLabelSelector(deployment.Spec.Selector.MatchLabels)}
		pods, err := k8s.GetPods(options)
		if err != nil {
			continue
		}
		// we are interested only in one instance of a pod
		if len(pods) > 0 {
			uniquePods = append(uniquePods, pods[0])
		}
		for _, pod := range pods {
			deploymentPods[pod.Name]++
		}
	}

	var statefulSetsPods map[string]int = make(map[string]int)
	statefulSets, err := k8s.GetStatefulSets()
	if err != nil {
		return 0, nil, err
	}

	for _, statefulSet := range statefulSets.Items {
		// to find all pods that are part of a given deployment we need to use statefulSet.Spec.Selector.MatchLabels
		// from the deployment. This is essential.
		options := metaV1.ListOptions{LabelSelector: mapToLabelSelector(statefulSet.Spec.Selector.MatchLabels)}
		pods, err := k8s.GetPods(options)
		if err != nil {
			continue
		}
		// we are interested only in one instance of a pod
		//podCount += len(pods)
		if len(pods) > 0 {
			uniquePods = append(uniquePods, pods[0])
		}
		for _, pod := range pods {
			statefulSetsPods[pod.Name]++
		}
	}

	var daemonSetsPods map[string]int = make(map[string]int)
	daemonSets, err := k8s.GetDaemonSets()
	if err != nil {
		return 0, nil, err
	}

	for _, daemonSet := range daemonSets.Items {
		// to find all pods that are part of a given deployment we need to use statefulSet.Spec.Selector.MatchLabels
		// from the deployment. This is essential.
		options := metaV1.ListOptions{LabelSelector: mapToLabelSelector(daemonSet.Spec.Selector.MatchLabels)}
		pods, err := k8s.GetPods(options)
		if err != nil {
			continue
		}
		// we are interested only in one instance of a pod
		//podCount += len(pods)
		if len(pods) > 0 {
			uniquePods = append(uniquePods, pods[0])
		}
		for _, pod := range pods {
			daemonSetsPods[pod.Name]++
		}
	}

	podsList, err := k8s.Clientset.CoreV1().Pods(k8s.Namespace).List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return 0, nil, err
	}
	for _, pod := range podsList.Items {
		if _, ok := deploymentPods[pod.Name]; ok {
			continue
		}
		if _, ok := statefulSetsPods[pod.Name]; ok {
			continue
		}
		if _, ok := daemonSetsPods[pod.Name]; ok {
			continue
		}
		uniquePods = append(uniquePods, pod)
	}

	return len(podsList.Items), uniquePods, nil
}

// GetUniquePods retrieves a comprehensive and unique list of Pods within a given namespace,
// as provided by the 'k8s' context. It targets Pods associated with Deployments, StatefulSets,
// and those directly within the namespace, ensuring no duplicates.
func (k8s *K8SExec) GetUniqueImages() (int, []string, error) {
	var images []string
	var containersCount int

	podsList, err := k8s.Clientset.CoreV1().Pods(k8s.Namespace).List(context.TODO(), metaV1.ListOptions{})
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

func (k8s *K8SExec) ReadFile(podName, containerName string, filePath string) (string, error) {
	var stdout, stderr bytes.Buffer
	ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFunc()

	retCode, err := k8s.exec(ctx, podName, containerName, []string{"cat", filePath}, nil, &stdout, &stderr, false)
	if retCode != Success {
		retCode, err = k8s.exec(ctx, podName, containerName, []string{"sed", "", filePath}, nil, &stdout, &stderr, false)
	}
	if retCode != Success {
		retCode, err = k8s.exec(ctx, podName, containerName, []string{"tail", "-n", "+1", filePath}, nil, &stdout, &stderr, false)
	}
	if retCode != Success {
		command := []string{
			"sh", "-c",
			fmt.Sprintf("while IFS= read -r line; do echo \"$line\"; done < '%s'", filePath),
		}
		retCode, err = k8s.exec(ctx, podName, containerName, command, nil, &stdout, &stderr, false)
	}
	return stdout.String(), err
}

func (k8s *K8SExec) CheckIfFilePathIsReadable(podName, containerName string, filePath string) bool {
	var stdout, stderr bytes.Buffer
	ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFunc()

	retCode, _ := k8s.exec(ctx, podName, containerName, []string{"stat", "-c", "%a", filePath}, nil, &stdout, &stderr, false)

	if retCode != Success {
		retCode, _ = k8s.exec(ctx, podName, containerName, []string{"sh", "-c", fmt.Sprintf("test -r '%s'", filePath)}, nil, &stdout, &stderr, false)

		return retCode == Success
	}
	permStr := stdout.String()
	if len(permStr) >= 3 {
		if len(permStr) == 4 {
			permStr = permStr[1:]
		}

		ownerPerm := int(permStr[0] - '0')
		groupPerm := int(permStr[1] - '0')
		othersPerm := int(permStr[2] - '0')
		const readBit = 4

		return ownerPerm&readBit != 0 || groupPerm&readBit != 0 || othersPerm&readBit != 0
	}

	return false
}

func (k8s *K8SExec) CheckIfFilePathExists(podName, containerName string, filePath string) bool {
	var stdout, stderr bytes.Buffer
	ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFunc()

	retCode, _ := k8s.exec(ctx, podName, containerName, []string{"stat", filePath}, nil, &stdout, &stderr, false)
	if retCode != Success {
		retCode, _ = k8s.exec(ctx, podName, containerName, []string{"sh", "-c", fmt.Sprintf("[ -f '%s' ]", filePath)}, nil, &stdout, &stderr, false)
	}

	return retCode == Success
}

// CheckUtilInContainer verifies the existence of a specified 'util' binary within a container, identified
// by the container's name and the associated pod's name.
func (k8s *K8SExec) CheckUtilInContainer(podName, containerName string, util string) bool {
	var stdout, stderr bytes.Buffer
	ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFunc()

	retCode, _ := k8s.exec(ctx, podName, containerName, []string{util}, nil, &stdout, &stderr, false)
	// TODO: Maybe it would make sense to make it a positive check for successful execution instead of a negative one
	return retCode != CommandNotFound && retCode != CommandCannotExecute && retCode != InternalAppError
}

// exec executes a command provided via standard input ('stdin'), command-line arguments ('cmd'),
// or both, offering a versatile interface for command execution. Upon completion, it returns a POSIX
// execution code to indicate the success or failure of the operation, alongside any error encountered
// during execution for detailed diagnostics. Additionally, the function captures and returns both
// the standard output ('stdout') and standard error ('stderr') streams, providing details of the command's execution.
func (k8s *K8SExec) exec(ctx context.Context, podName string, containerName string, cmd []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, tty bool) (ExitCode, error) {
	req := k8s.Clientset.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace(k8s.Namespace).
		SubResource("exec").
		VersionedParams(&coreV1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    stdout != nil,
			Stderr:    stderr != nil,
			TTY:       tty,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(k8s.Config, "POST", req.URL())
	if err != nil {
		return InternalAppError, err
	}

	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    false,
	})
	if err != nil {
		exitError := exec2.CodeExitError{}
		if errors.As(err, &exitError) {
			return ExitCode(exitError.Code), exitError
		}

		return InternalAppError, err
	}

	return Success, nil
}

// NewExecutionStatus initializes a new instance of the ExecutionStatus type, providing a method
// to encapsulate the outcome of a command's execution within a structured format.
// This function serves as a constructor, setting up an ExecutionStatus instance.
func NewExecutionStatus(pod string, container string, retCode ExitCode, error string, stdout string, stderr string) *ExecutionStatus {
	return &ExecutionStatus{Pod: pod, Container: container, RetCode: retCode, Error: strings.Split(error, "\n"), Stdout: strings.Split(stdout, "\n"), Stderr: strings.Split(stderr, "\n")}
}

// Exec executes a command provided through standard input ('stdin') or as arguments ('args'),
// or a combination of both. This function returns a pointer to an instance of ExecutionStatus,
// which encapsulates the results of the command execution. This includes details such as the exit code,
// error messages, and the outputs captured from both the standard output and standard error streams.
// timeout has to be provided as time.Duration.
func (k8s *K8SExec) Exec(podName string, containerName string, args []string, stdin io.Reader, timeout time.Duration) *ExecutionStatus {
	var stdout, stderr bytes.Buffer
	var errMessage string

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// ----- debug ----
	//var buffer bytes.Buffer
	//tee := io.TeeReader(stdin, &buffer)
	//_, _ = io.ReadAll(tee)
	//fmt.Println(buffer.String())
	//stdin = bytes.NewReader(buffer.Bytes())
	// ----- debug ----

	retCode, err := k8s.exec(ctx, podName, containerName, args, stdin, &stdout, &stderr, false)
	if err != nil {
		errMessage = err.Error()
	}

	if errors.Is(err, context.DeadlineExceeded) {
		retCode = ExecutionTimeOut
	}
	return NewExecutionStatus(podName, containerName, retCode, errMessage, stdout.String(), stderr.String())
}

// ExecWithContext executes a command provided through standard input ('stdin') or as arguments ('args'),
// or a combination of both. This function returns a pointer to an instance of ExecutionStatus,
// which encapsulates the results of the command execution. This includes details such as the exit code,
// error messages, and the outputs captured from both the standard output and standard error streams.
// The use of this function must provide a context that will govern the command exeuction.
func (k8s *K8SExec) ExecWithContext(ctx context.Context, podName string, containerName string, args []string, stdin io.Reader) *ExecutionStatus {
	var stdout, stderr bytes.Buffer
	var errMessage string

	retCode, err := k8s.exec(ctx, podName, containerName, args, stdin, &stdout, &stderr, false)
	if err != nil {
		errMessage = err.Error()
	}
	return NewExecutionStatus(podName, containerName, retCode, errMessage, stdout.String(), stderr.String())
}
