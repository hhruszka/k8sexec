package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"

	"path/filepath"
	"strings"

	"github.com/hhruszka/k8sexec"
)

type Enviroment []string

func (e *Enviroment) String() string {
	return strings.Join(*e, ",")
}

func (e *Enviroment) Set(value string) error {
	*e = append(*e, value)
	return nil
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s <pod name> [flags] -- <command>\n", os.Args[0])
	fmt.Fprint(os.Stderr, "Flags:\n")
	fmt.Fprint(os.Stderr, "  -c, --container <container name>       Container name to execute command on.\n")
	fmt.Fprint(os.Stderr, "  -e, --env <VAR=value>                  Environment variables to use during execution.Can be specified multiple times.\n")
	fmt.Fprint(os.Stderr, "  -k, --kubeconfig <path to kubeconfig>  Path to a kubeconfig. Only required if out-of-cluster.\n")
	fmt.Fprint(os.Stderr, "  -n, --namespace <namespace>            Namespace to use for Kubernetes API requests.\n")
	fmt.Fprint(os.Stderr, "  -h, --help                             Show this help message and exit.\n")
	fmt.Fprint(os.Stderr, "Examples:\n")
	fmt.Fprint(os.Stderr, "  # Execute 'ls' command in 'nginx' container in 'default' namespace.\n")
	fmt.Fprint(os.Stderr, "  kubectl exec nginx -c nginx -- ls\n")
}

func main() {
	var (
		kubeconfig string
		namespace  string
		podName    string
		container  string
		enviroment Enviroment
	)

	if len(os.Args) < 2 {
		//fmt.Fprintf(os.Stderr, "Usage: %s <pod name> [flags] -- <command>\n", os.Args[0])
		usage()
		os.Exit(1)
	}

	podName = os.Args[1]

	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	homeDir, _ := os.UserHomeDir()
	kubeconfig = filepath.Join(homeDir, ".kube", "config")

	fs.StringVar(&kubeconfig, "kubeconfig", kubeconfig, "Path to a kubeconfig. Only required if out-of-cluster.")
	fs.StringVar(&kubeconfig, "k", kubeconfig, "Path to a kubeconfig. Only required if out-of-cluster.")
	fs.StringVar(&namespace, "namespace", "default", "Namespace to use for Kubernetes API requests.")
	fs.StringVar(&namespace, "n", "default", "Namespace to use for Kubernetes API requests.")
	//flag.StringVar(&podName, "pod", "", "Pod name to execute command on.")
	//flag.StringVar(&podName, "p", "", "Pod name to execute command on.")
	fs.Var(&enviroment, "env", "Environment variables to use during execution.Can be specified multiple times.")
	fs.Var(&enviroment, "e", "Environment variables to use during execution.Can be specified multiple times.")
	fs.StringVar(&container, "container", "", "Container name to execute command on.")
	fs.StringVar(&container, "c", "", "Container name to execute command on.")
	fs.Usage = usage
	flag.Usage = fs.Usage

	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing flags: %v\n", err)
		os.Exit(1)
	}
	flag.Parse()

	if len(enviroment) > 0 {
		fmt.Fprint(os.Stderr, "Flag --env/-e is not implemented")
	}

	command := fs.Args()

	if _, err := os.Stat(kubeconfig); os.IsNotExist(err) {
		fmt.Println("Kubeconfig file not found")
		os.Exit(1)
	}

	if len(command) == 0 {
		fmt.Println("No command specified")
		os.Exit(1)
	}

	k8s, err := k8sexec.NewK8SExec(kubeconfig)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	if container == "" {
		defaultContainer, err := k8s.GetPodDefaultContainer(context.Background(), namespace, podName)
		if err != nil {
			fmt.Fprintln(os.Stderr, fmt.Errorf("Failed to retrieve default container for pod %s; %w", podName, err))
			os.Exit(1)
		}
		container = defaultContainer.Name
	}

	var stdout, stderr bytes.Buffer
	exitCode, err := k8s.DirectExec(context.Background(), namespace, podName, container, command, os.Stdin, &stdout, &stderr, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		// Without an exit status from the command there is nothing meaningful to
		// propagate, and exiting 0 here would report a failed connection as success.
		if _, _, ok := k8sexec.GetExitCode(err); !ok {
			os.Exit(1)
		}
	}
	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("Command: %v\n", command)
	fmt.Println("Exit code: ", exitCode)
	fmt.Fprint(os.Stdout, "Stdout:\n", stdout.String())
	fmt.Fprint(os.Stderr, "Stderr:\n", stderr.String())
	os.Exit(int(exitCode))
}
