package k8sexec

import (
	"context"
	"testing"
	"time"

	appsV1 "k8s.io/api/apps/v1"
	batchV1 "k8s.io/api/batch/v1"
	coreV1 "k8s.io/api/core/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newFakeExec builds a K8SExec backed by the fake clientset, seeded with the given objects.
func newFakeExec(objects ...runtime.Object) *K8SExec {
	return &K8SExec{Clientset: fake.NewSimpleClientset(objects...)}
}

func namespace(name string) *coreV1.Namespace {
	return &coreV1.Namespace{ObjectMeta: metaV1.ObjectMeta{Name: name}}
}

// pod builds a Pod in the given namespace with the supplied containers.
func pod(ns, name string, containers ...coreV1.Container) *coreV1.Pod {
	return &coreV1.Pod{
		ObjectMeta: metaV1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       coreV1.PodSpec{Containers: containers},
	}
}

func container(name, image string) coreV1.Container {
	return coreV1.Container{Name: name, Image: image}
}

func TestGetAllNamespaces(t *testing.T) {
	k8s := newFakeExec(namespace("default"), namespace("kube-system"), namespace("app"))

	got, err := k8s.GetAllNamespaces(context.Background())
	if err != nil {
		t.Fatalf("GetAllNamespaces() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("GetAllNamespaces() returned %d namespaces (%v), want 3", len(got), got)
	}

	seen := map[string]bool{}
	for _, ns := range got {
		seen[ns] = true
	}
	for _, want := range []string{"default", "kube-system", "app"} {
		if !seen[want] {
			t.Errorf("GetAllNamespaces() missing %q, got %v", want, got)
		}
	}
}

func TestGetAllNamespacesEmpty(t *testing.T) {
	k8s := newFakeExec()

	got, err := k8s.GetAllNamespaces(context.Background())
	if err != nil {
		t.Fatalf("GetAllNamespaces() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("GetAllNamespaces() = %v, want empty", got)
	}
}

func TestGetPod(t *testing.T) {
	k8s := newFakeExec(pod("ns1", "web", container("app", "nginx:1.25")))

	got, err := k8s.GetPod(context.Background(), "ns1", "web")
	if err != nil {
		t.Fatalf("GetPod() error = %v", err)
	}
	if got.Name != "web" || got.Namespace != "ns1" {
		t.Errorf("GetPod() = %s/%s, want ns1/web", got.Namespace, got.Name)
	}
}

// TestGetPodIsNamespaceScoped guards against a Pod being fetched from the wrong namespace.
func TestGetPodIsNamespaceScoped(t *testing.T) {
	k8s := newFakeExec(pod("ns1", "web", container("app", "nginx:1.25")))

	if _, err := k8s.GetPod(context.Background(), "ns2", "web"); err == nil {
		t.Error("GetPod() in the wrong namespace returned no error, want NotFound")
	}
}

func TestGetPodNotFound(t *testing.T) {
	k8s := newFakeExec()

	if _, err := k8s.GetPod(context.Background(), "ns1", "missing"); err == nil {
		t.Error("GetPod() for a missing Pod returned no error, want NotFound")
	}
}

func TestGetPodContainers(t *testing.T) {
	k8s := newFakeExec(pod("ns1", "web", container("app", "nginx:1.25"), container("sidecar", "envoy:1.30")))

	got, err := k8s.GetPodContainers(context.Background(), "ns1", "web")
	if err != nil {
		t.Fatalf("GetPodContainers() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetPodContainers() returned %d containers, want 2", len(got))
	}
	if got[0].Name != "app" || got[1].Name != "sidecar" {
		t.Errorf("GetPodContainers() = [%s %s], want [app sidecar]", got[0].Name, got[1].Name)
	}
}

func TestGetPodContainer(t *testing.T) {
	k8s := newFakeExec(pod("ns1", "web", container("app", "nginx:1.25"), container("sidecar", "envoy:1.30")))

	got, err := k8s.GetPodContainer(context.Background(), "ns1", "web", "sidecar")
	if err != nil {
		t.Fatalf("GetPodContainer() error = %v", err)
	}
	if got.Name != "sidecar" || got.Image != "envoy:1.30" {
		t.Errorf("GetPodContainer() = %s/%s, want sidecar/envoy:1.30", got.Name, got.Image)
	}
}

func TestGetPodContainerNotFound(t *testing.T) {
	k8s := newFakeExec(pod("ns1", "web", container("app", "nginx:1.25")))

	if _, err := k8s.GetPodContainer(context.Background(), "ns1", "web", "nope"); err == nil {
		t.Error("GetPodContainer() for a missing container returned no error")
	}
}

// TestGetPodDefaultContainer covers the annotation-driven selection and its fallbacks.
func TestGetPodDefaultContainer(t *testing.T) {
	annotated := pod("ns1", "web", container("app", "nginx:1.25"), container("sidecar", "envoy:1.30"))
	annotated.Annotations = map[string]string{"kubectl.kubernetes.io/default-container": "sidecar"}

	// An annotation naming a container that does not exist must fall back to the first.
	stale := pod("ns1", "stale", container("app", "nginx:1.25"))
	stale.Annotations = map[string]string{"kubectl.kubernetes.io/default-container": "ghost"}

	tests := []struct {
		name    string
		pod     *coreV1.Pod
		podName string
		want    string
	}{
		{"annotation selects the named container", annotated, "web", "sidecar"},
		{"no annotation falls back to the first container", pod("ns1", "plain", container("first", "a:1"), container("second", "b:1")), "plain", "first"},
		{"stale annotation falls back to the first container", stale, "stale", "app"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8s := newFakeExec(tt.pod)
			got, err := k8s.GetPodDefaultContainer(context.Background(), "ns1", tt.podName)
			if err != nil {
				t.Fatalf("GetPodDefaultContainer() error = %v", err)
			}
			if got.Name != tt.want {
				t.Errorf("GetPodDefaultContainer() = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestGetPodDefaultContainerNoContainers(t *testing.T) {
	k8s := newFakeExec(pod("ns1", "empty"))

	if _, err := k8s.GetPodDefaultContainer(context.Background(), "ns1", "empty"); err == nil {
		t.Error("GetPodDefaultContainer() on a Pod with no containers returned no error")
	}
}

func TestGetPods(t *testing.T) {
	k8s := newFakeExec(
		pod("ns1", "a", container("c", "img:1")),
		pod("ns1", "b", container("c", "img:1")),
		pod("ns2", "c", container("c", "img:1")),
	)

	got, err := k8s.GetPods(context.Background(), "ns1", metaV1.ListOptions{})
	if err != nil {
		t.Fatalf("GetPods() error = %v", err)
	}
	if len(got) != 2 {
		t.Errorf("GetPods() returned %d Pods, want 2 (namespace scoping)", len(got))
	}
}

func TestGetJobs(t *testing.T) {
	job := &batchV1.Job{ObjectMeta: metaV1.ObjectMeta{Name: "migrate", Namespace: "ns1"}}
	k8s := newFakeExec(job)

	got, err := k8s.GetJobs(context.Background(), "ns1", metaV1.ListOptions{})
	if err != nil {
		t.Fatalf("GetJobs() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != "migrate" {
		t.Errorf("GetJobs() = %v, want one Job named migrate", got)
	}
}

func TestGetWorkloadListers(t *testing.T) {
	k8s := newFakeExec(
		&appsV1.Deployment{ObjectMeta: metaV1.ObjectMeta{Name: "web", Namespace: "ns1"}},
		&appsV1.StatefulSet{ObjectMeta: metaV1.ObjectMeta{Name: "db", Namespace: "ns1"}},
		&appsV1.DaemonSet{ObjectMeta: metaV1.ObjectMeta{Name: "agent", Namespace: "ns1"}},
		&coreV1.ConfigMap{ObjectMeta: metaV1.ObjectMeta{Name: "cfg", Namespace: "ns1"}},
	)
	ctx := context.Background()

	deployments, err := k8s.GetDeployments(ctx, "ns1")
	if err != nil || len(deployments.Items) != 1 {
		t.Errorf("GetDeployments() = %v, err = %v; want one item", deployments, err)
	}
	statefulSets, err := k8s.GetStatefulSets(ctx, "ns1")
	if err != nil || len(statefulSets.Items) != 1 {
		t.Errorf("GetStatefulSets() = %v, err = %v; want one item", statefulSets, err)
	}
	daemonSets, err := k8s.GetDaemonSets(ctx, "ns1")
	if err != nil || len(daemonSets.Items) != 1 {
		t.Errorf("GetDaemonSets() = %v, err = %v; want one item", daemonSets, err)
	}
	configMaps, err := k8s.GetConfigmaps(ctx, "ns1")
	if err != nil || len(configMaps.Items) != 1 {
		t.Errorf("GetConfigmaps() = %v, err = %v; want one item", configMaps, err)
	}
}

func TestGetUniqueImages(t *testing.T) {
	k8s := newFakeExec(
		pod("ns1", "a", container("c1", "nginx:1.25"), container("c2", "envoy:1.30")),
		pod("ns1", "b", container("c1", "nginx:1.25")), // duplicate image
		pod("ns1", "c", container("c1", "redis:7")),
	)

	count, images, err := k8s.GetUniqueImages(context.Background(), "ns1")
	if err != nil {
		t.Fatalf("GetUniqueImages() error = %v", err)
	}
	if count != 4 {
		t.Errorf("container count = %d, want 4 (every container, duplicates included)", count)
	}
	if len(images) != 3 {
		t.Errorf("images = %v, want 3 unique", images)
	}
}

// replicaSet builds a ReplicaSet, optionally controlled by a Deployment.
func replicaSet(ns, name string, uid types.UID, deploymentUID types.UID) *appsV1.ReplicaSet {
	rs := &appsV1.ReplicaSet{ObjectMeta: metaV1.ObjectMeta{Name: name, Namespace: ns, UID: uid}}
	if deploymentUID != "" {
		rs.OwnerReferences = []metaV1.OwnerReference{controllerRef("Deployment", "deploy", deploymentUID)}
	}
	return rs
}

// ownedPod builds a running Pod controlled by the given owner.
func ownedPod(ns, name string, owner metaV1.OwnerReference, created time.Time) *coreV1.Pod {
	p := runningPod(name, created)
	p.Namespace = ns
	p.OwnerReferences = []metaV1.OwnerReference{owner}
	return p
}

func TestReplicaSetOwners(t *testing.T) {
	k8s := newFakeExec(
		replicaSet("ns1", "web-old", "rs-old", "deploy-1"),
		replicaSet("ns1", "web-new", "rs-new", "deploy-1"),
		replicaSet("ns1", "bare", "rs-bare", ""), // no controlling Deployment
	)

	owners, err := k8s.replicaSetOwners(context.Background(), "ns1")
	if err != nil {
		t.Fatalf("replicaSetOwners() error = %v", err)
	}
	if owners["rs-old"] != "deploy-1" || owners["rs-new"] != "deploy-1" {
		t.Errorf("owners = %v, want rs-old and rs-new mapped to deploy-1", owners)
	}
	if _, ok := owners["rs-bare"]; ok {
		t.Errorf("owners contains rs-bare = %v, want an uncontrolled ReplicaSet to be absent", owners["rs-bare"])
	}
}

// TestGetUniquePodsCollapsesRollout is the end-to-end guard for the workload-grouping fix:
// a Deployment mid-rollout has Pods under two ReplicaSets and must yield a single Pod.
func TestGetUniquePodsCollapsesRollout(t *testing.T) {
	now := time.Now()
	oldRef := controllerRef("ReplicaSet", "web-old", "rs-old")
	newRef := controllerRef("ReplicaSet", "web-new", "rs-new")
	stsRef := controllerRef("StatefulSet", "db", "sts-1")

	k8s := newFakeExec(
		replicaSet("ns1", "web-old", "rs-old", "deploy-1"),
		replicaSet("ns1", "web-new", "rs-new", "deploy-1"),
		ownedPod("ns1", "web-old-xxxx", oldRef, now.Add(-time.Hour)),
		ownedPod("ns1", "web-new-yyyy", newRef, now),
		ownedPod("ns1", "db-0", stsRef, now),
		pod("ns1", "standalone", container("c", "img:1")), // no controller
	)

	total, unique, err := k8s.GetUniquePods(context.Background(), "ns1")
	if err != nil {
		t.Fatalf("GetUniquePods() error = %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4 live Pods", total)
	}

	// Expect one Pod for the Deployment, one for the StatefulSet, one standalone.
	if len(unique) != 3 {
		names := make([]string, len(unique))
		for i, p := range unique {
			names[i] = p.Name
		}
		t.Fatalf("unique = %v (%d), want 3: the rollout must collapse to a single Pod", names, len(unique))
	}

	// The newer ReplicaSet's Pod is preferred: equal score, later CreationTimestamp.
	var sawDeploymentPod bool
	for _, p := range unique {
		if p.Name == "web-new-yyyy" {
			sawDeploymentPod = true
		}
		if p.Name == "web-old-xxxx" {
			t.Errorf("unique contains the older rollout Pod %q, want the newer one", p.Name)
		}
	}
	if !sawDeploymentPod {
		t.Error("unique is missing the Deployment representative web-new-yyyy")
	}
}

// TestGetUniquePodsSurvivesReplicaSetListFailure pins the documented degraded path: when the
// ReplicaSet List fails, grouping falls back to the immediate controller instead of erroring.
func TestGetUniquePodsSurvivesReplicaSetListFailure(t *testing.T) {
	now := time.Now()
	oldRef := controllerRef("ReplicaSet", "web-old", "rs-old")
	newRef := controllerRef("ReplicaSet", "web-new", "rs-new")

	clientset := fake.NewSimpleClientset(
		ownedPod("ns1", "web-old-xxxx", oldRef, now.Add(-time.Hour)),
		ownedPod("ns1", "web-new-yyyy", newRef, now),
	)
	clientset.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	k8s := &K8SExec{Clientset: clientset}

	_, unique, err := k8s.GetUniquePods(context.Background(), "ns1")
	if err != nil {
		t.Fatalf("GetUniquePods() error = %v, want the ReplicaSet List failure to be non-fatal", err)
	}
	// Without the owner map, the two ReplicaSets cannot be collapsed, so both Pods survive.
	if len(unique) != 2 {
		t.Errorf("unique = %d Pods, want 2 (degraded per-ReplicaSet grouping)", len(unique))
	}
}

func TestGetUniquePodsSortedByName(t *testing.T) {
	k8s := newFakeExec(
		pod("ns1", "zebra", container("c", "img:1")),
		pod("ns1", "alpha", container("c", "img:1")),
		pod("ns1", "mango", container("c", "img:1")),
	)

	_, unique, err := k8s.GetUniquePods(context.Background(), "ns1")
	if err != nil {
		t.Fatalf("GetUniquePods() error = %v", err)
	}
	for i := 1; i < len(unique); i++ {
		if unique[i-1].Name > unique[i].Name {
			t.Errorf("unique is not sorted by name: %q before %q", unique[i-1].Name, unique[i].Name)
		}
	}
}

func TestGetUniquePodsPropagatesPodListError(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})
	k8s := &K8SExec{Clientset: clientset}

	if _, _, err := k8s.GetUniquePods(context.Background(), "ns1"); err == nil {
		t.Error("GetUniquePods() returned no error when the Pod List failed")
	}
}
