package provider

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Fake Kubernetes clients do not enforce resourceVersion CAS. This test uses
// real etcd and kube-apiserver to exercise the duplicate-prevention invariant
// under competing provider processes, including CR pinning and journal CAS.
// CI provides pinned envtest binaries and therefore always runs this test.
func TestConcurrentProvidersAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run the real Kubernetes API concurrency test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	environment := &envtest.Environment{CRDDirectoryPaths: []string{"testdata"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start real API server: %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop API server: %v", err)
		}
	})
	config.QPS, config.Burst = 100, 200
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	const namespace = "provider-concurrency-test"
	if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	node, err := kube.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-worker-1", Labels: map[string]string{DefaultNodeNameLabel: "compute-machine-42"}},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node.Status.NodeInfo.BootID = "boot-before-concurrent-reboot"
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	node, err = kube.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	const clients = 12
	var calls atomic.Int32
	rebooter := rebootFunc(func(context.Context, string) error { calls.Add(1); return nil })
	raceReboots := func(offset int) string {
		for i := range clients / 2 {
			cr := testCR(fmt.Sprintf("reboot-%d", offset+i), "", node.Name)
			cr.SetResourceVersion("")
			if _, err := dynamicClient.Resource(rebootNodes).Create(ctx, cr, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		start := make(chan struct{})
		refs := make([]string, clients)
		errors := make([]error, clients)
		var wg sync.WaitGroup
		for i := range clients {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p, err := New(Config{Namespace: namespace, Scope: "integration-compute-scope"}, kube, dynamicClient, rebooter)
				if err != nil {
					errors[i] = err
					return
				}
				<-start
				// Model Janitor's retry of an RPC when many writers exceed the
				// provider's bounded optimistic transaction retry budget.
				for attempt := range 10 {
					response, err := p.SendRebootSignal(ctx, &cspv1alpha1.SendRebootSignalRequest{NodeName: node.Name, CrName: fmt.Sprintf("reboot-%d", offset+i/2)})
					if err == nil {
						refs[i] = response.RequestId
						return
					}
					if status.Code(err) != codes.Unavailable || attempt == 9 {
						errors[i] = err
						return
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		for i := range clients {
			if errors[i] != nil || refs[i] == "" || refs[i] != refs[0] {
				t.Errorf("replica %d: ref=%q first=%q error=%v", i, refs[i], refs[0], errors[i])
			}
		}
		return refs[0]
	}
	first := raceReboots(0) // Exercise concurrent ConfigMap Create.
	if calls.Load() != 1 {
		t.Fatalf("competing API transactions dispatched %d reboots; want exactly one in this successful-send test", calls.Load())
	}
	cm, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, journalName(node.UID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	j, err := decodeJournal(cm)
	if err != nil || len(j.Operations) != 1 || len(j.Bindings) != clients/2 {
		t.Fatalf("durable journal after concurrency: %+v error=%v", j, err)
	}
	if j.Operations[j.Current].Phase != phaseSubmitted {
		t.Fatalf("journal phase = %q, want submitted", j.Operations[j.Current].Phase)
	}
	node, err = kube.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node.Status.NodeInfo.BootID = "boot-after-concurrent-reboot"
	if _, err := kube.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	second := raceReboots(clients / 2) // Exercise concurrent ConfigMap Update CAS.
	if first == second || calls.Load() != 2 {
		t.Fatalf("new boot update race: first=%q second=%q calls=%d", first, second, calls.Load())
	}
	cm, err = kube.CoreV1().ConfigMaps(namespace).Get(ctx, journalName(node.UID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	j, err = decodeJournal(cm)
	if err != nil || len(j.Operations) != 2 || len(j.Bindings) != clients {
		t.Fatalf("durable journal after update concurrency: %+v error=%v", j, err)
	}
}
