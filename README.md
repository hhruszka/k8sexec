# k8sexec

k8sexec module is based on the k8s.io framework. Its major purpose is to execute commands in containers. k8sexec.Exec() method
executes a command, commands or scripts provided through standard input ('stdin') or as arguments ('args'), or a combination of both. 
It returns a pointer to an instance of ExecutionStatus, which encapsulates the results of the command execution. 
This includes details such as the exit code, error messages, and the outputs captured from both the standard output and 
standard error streams.

It must be mentioned that k8sexec.Exec() allows to execute all kinds of scripts/commands through stdin. Below example shows how to embed lse.sh
(Linux Smart Enumeration) script into a go binary and then execute it through stdin.

EXAMPLE:
```go
package simpleexec

import (
	"github.com/hhruszka/k8sexec"
	"fmt"
	_ "embed"
	v1 "k8s.io/api/core/v1"
	"bytes"
)

//go:embed lse.sh
var lse []byte

func test(kubeconfig string, namespace string)  []*k8sexec.ExecutionStatus {
	var results []*k8sexec.ExecutionStatus
		
	k8s,err := k8sexec.NewK8SExec(kubeconfig,namespace)
	if err!= nil {
		return nil
    }
	
	cnt,pods,err := k8s.GetUniquePods()
	if err != nil {
		return nil
    }
	
	fmt.Printf("Found %d pods\n",cnt)
	
	for _, pod := range pods {
	    for _,container := range pod.Spec.Containers {
            lsescript := bytes.NewBuffer(lse)
			
            result := k8s.Exec(pod.Name, container.Name, []string{"sh"}, lsescript)
            results = append(results,result)
	    }
	}
	
	return results
}
```

Additionally, k8sexec module provides functions for retrieving pods, deployments and statefulset that can be used to 
automate enumeration of containers or any other information.
