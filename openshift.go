package k8sexec

import (
	"context"
	"fmt"

	v1 "github.com/openshift/api/security/v1"
	securityv1 "github.com/openshift/client-go/security/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsOpenShift determines if the cluster is running OpenShift by checking for specific OpenShift API groups.
func (k8s *K8SExec) IsOpenShift() bool {
	// OpenShift always registers this API group
	groups, err := k8s.Clientset.Discovery().ServerGroups()
	if err != nil {
		return false
	}

	for _, group := range groups.Groups {
		if group.Name == "route.openshift.io" || group.Name == "security.openshift.io" {
			return true
		}
	}
	return false
}

// GetSCC retrieves the SecurityContextConstraints (SCC) named "restricted-v2" from an OpenShift cluster.
// Returns the SCC object or an error if the cluster is not OpenShift or if retrieval fails.
func (k8s *K8SExec) GetSCC() (*v1.SecurityContextConstraints, error) {
	if !k8s.IsOpenShift() {
		return nil, fmt.Errorf("cluster is not running OpenShift")
	}
	ocSecurityClient, err := securityv1.NewForConfig(k8s.Config)
	if err != nil {
		return nil, fmt.Errorf("error creating OpenShift Security Client: %w", err)
	}

	sccName := "restricted-v2"
	scc, err := ocSecurityClient.SecurityV1().SecurityContextConstraints().Get(context.TODO(), sccName, metav1.GetOptions{})

	if err != nil {
		return nil, fmt.Errorf("error retrieving SCC: %w", err)
	}
	return scc, nil
}
