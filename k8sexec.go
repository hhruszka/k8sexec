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
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	exec2 "k8s.io/client-go/util/exec"
	"strings"
)

type ExecutionStatus struct {
	Pod       string   `json:"Pod"`
	Container string   `json:"Container"`
	RetCode   int      `json:"RetCode"`
	Error     []string `json:"Error"`
	Stdout    []string `json:"Stdout"`
	Stderr    []string `json:"Stderr"`
}

// App global variables
type K8SExec struct {
	Config    *rest.Config
	Clientset *kubernetes.Clientset
	Namespace string
}

var ExitCodes map[int]string = map[int]string{
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

func GetExitCode(err error) (int, string) {
	var e exec2.CodeExitError
	if !errors.As(err, &e) {
		return -1, ""
	}
	if _, ok := ExitCodes[e.Code]; !ok {
		return e.Code, fmt.Sprintf("Exit code %d description not found!", e.Code)
	}
	return e.Code, ExitCodes[e.Code]
}

func GetExitCodeDescription(code int) string {
	if _, ok := ExitCodes[code]; !ok {
		return ""
	}
	return ExitCodes[code]
}

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

func (k8s *K8SExec) GetPod(podName string, options metaV1.GetOptions) (*coreV1.Pod, error) {
	pod, err := k8s.Clientset.CoreV1().Pods(k8s.Namespace).Get(context.TODO(), podName, metaV1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return pod, nil
}

func (k8s *K8SExec) GetPods(options metaV1.ListOptions) ([]coreV1.Pod, error) {
	var pods *coreV1.PodList
	pods, err := k8s.Clientset.CoreV1().Pods(k8s.Namespace).List(context.TODO(), options)
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func (k8s *K8SExec) GetDeployments() (*v1.DeploymentList, error) {
	var deployments *v1.DeploymentList
	deployments, err := k8s.Clientset.AppsV1().Deployments(k8s.Namespace).List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return deployments, nil
}

func (k8s *K8SExec) GetStatefulSets() (*v1.StatefulSetList, error) {
	var statefulSets *v1.StatefulSetList
	statefulSets, err := k8s.Clientset.AppsV1().StatefulSets(k8s.Namespace).List(context.TODO(), metaV1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return statefulSets, nil
}

// mapToLabelSelector converts a map of key-value pairs to a Kubernetes label selector string.
func mapToLabelSelector(labels map[string]string) string {
	var selectorParts []string
	for key, value := range labels {
		selectorParts = append(selectorParts, fmt.Sprintf("%s=%s", key, value))
	}
	return strings.Join(selectorParts, ",")
}

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
	//log(fmt.Sprintf("[+] Found %d pods in %d deployments\n", len(deploymentPods), len(deployments.Items)))

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
	//log(fmt.Sprintf("[+] Found %d pods in %d statefulsets\n", len(statefulSetsPods), len(statefulSets.Items)))

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
		uniquePods = append(uniquePods, pod)
	}

	return len(podsList.Items), uniquePods, nil
}

func (k8s *K8SExec) CheckUtilInContainer(podName, containerName string, util string) bool {
	var stdout, stderr bytes.Buffer
	retCode, _ := k8s.exec(podName, containerName, []string{util}, nil, &stdout, &stderr, false)
	return retCode != 127 && retCode != 126
}

func (k8s *K8SExec) exec(podName string, containerName string, cmd []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, tty bool) (int, error) {

	//command := []string{cmd}

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
		return -1, err
	}

	err = executor.StreamWithContext(context.TODO(), remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    false,
	})
	if err != nil {
		exitError := exec2.CodeExitError{}
		if errors.As(err, &exitError) {
			return exitError.Code, exitError
		}
		return -1, err
	}

	return 0, nil
}

func NewExecutionStatus(pod string, container string, retCode int, error string, stdout string, stderr string) *ExecutionStatus {
	return &ExecutionStatus{Pod: pod, Container: container, RetCode: retCode, Error: strings.Split(error, "\n"), Stdout: strings.Split(stdout, "\n"), Stderr: strings.Split(stderr, "\n")}
}

func (k8s *K8SExec) Exec(podName string, containerName string, args []string, stdin io.Reader) *ExecutionStatus {
	var stdout, stderr bytes.Buffer
	var errMessage string

	retCode, err := k8s.exec(podName, containerName, args, stdin, &stdout, &stderr, false)
	if err != nil {
		errMessage = err.Error()
	}
	return NewExecutionStatus(podName, containerName, retCode, errMessage, stdout.String(), stderr.String())
}
