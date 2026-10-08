package provider

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

type rebootFunc func(context.Context, string) error

func (f rebootFunc) Reboot(ctx context.Context, target string) error { return f(ctx, target) }

type fixture struct {
	t       *testing.T
	node    *corev1.Node
	cr      *unstructured.Unstructured
	kube    *fake.Clientset
	dynamic *dynamicfake.FakeDynamicClient
	calls   atomic.Int32
	err     error
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-worker-1", UID: "node-uid-1", ResourceVersion: "1",
			Labels: map[string]string{DefaultNodeNameLabel: "compute-machine-42"}},
		Spec: corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{BootID: "boot-a"},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	cr := testCR("reboot-1", "cr-uid-1", node.Name)
	return &fixture{
		t: t, node: node, cr: cr, kube: fake.NewSimpleClientset(node),
		dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), cr),
	}
}

func testCR(name string, uid types.UID, nodeName string) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "janitor.dgxc.nvidia.com/v1alpha1", "kind": "RebootNode",
		"metadata": map[string]interface{}{"name": name, "uid": string(uid), "resourceVersion": "1"},
		"spec":     map[string]interface{}{"nodeName": nodeName, "force": false},
	}}
	return cr
}

func (f *fixture) provider() *Provider {
	f.t.Helper()
	p, err := New(Config{Namespace: "provider-system", Scope: "test-compute-scope"}, f.kube, f.dynamic, rebootFunc(func(_ context.Context, target string) error {
		f.calls.Add(1)
		if target != "compute-machine-42" {
			f.t.Errorf("target = %q, want explicit mapped machine", target)
		}
		return f.err
	}))
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) updateNode(change func(*corev1.Node)) {
	f.t.Helper()
	node, err := f.kube.CoreV1().Nodes().Get(context.Background(), f.node.Name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	change(node)
	if _, err := f.kube.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) addCR(name string) {
	f.t.Helper()
	if _, err := f.dynamic.Resource(rebootNodes).Create(context.Background(), testCR(name, types.UID(name+"-uid"), f.node.Name), metav1.CreateOptions{}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) send(p *Provider, cr string) string {
	f.t.Helper()
	response, err := p.SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: cr})
	if err != nil {
		f.t.Fatal(err)
	}
	if !referencePattern.MatchString(response.RequestId) {
		f.t.Fatalf("invalid reference %q", response.RequestId)
	}
	return response.RequestId
}

func (f *fixture) ready(p *Provider, ref string) (bool, error) {
	f.t.Helper()
	response, err := p.IsNodeReady(context.Background(), &cspv1alpha1.IsNodeReadyRequest{NodeName: f.node.Name, RequestId: ref})
	if err != nil {
		return false, err
	}
	return response.IsReady, nil
}

func TestMappingLabelRequiresProtectedNamespace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		label string
		valid bool
	}{
		{"default", "", true},
		{"explicit default", DefaultNodeNameLabel, true},
		{"reserved namespace", "node-restriction.kubernetes.io/node-name", true},
		{"custom protected namespace", "example.com.node-restriction.kubernetes.io/Compute_Name", true},
		{"old unprotected label", "nvsentinel.mistral.ai/node-name", false},
		{"unqualified", "node-name", false},
		{"deceptive prefix", "notnode-restriction.kubernetes.io/node-name", false},
		{"deceptive suffix", "node-restriction.kubernetes.io.example.com/node-name", false},
		{"protected text in name", "example.com/node-restriction.kubernetes.io", false},
		{"invalid qualified name", "example_com.node-restriction.kubernetes.io/node-name", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			p, err := New(Config{Namespace: "provider-system", NodeNameLabel: tc.label, Scope: "test-compute-scope"},
				f.kube, f.dynamic, rebootFunc(func(context.Context, string) error {
					t.Fatal("configuration validation must not send a reboot")
					return nil
				}))
			if (err == nil) != tc.valid {
				t.Fatalf("label %q: error=%v, want valid=%v", tc.label, err, tc.valid)
			}
			if tc.valid && tc.label == "" && p.config.NodeNameLabel != DefaultNodeNameLabel {
				t.Fatalf("unexpected default label: %q", p.config.NodeNameLabel)
			}
		})
	}
}

func TestDurableReplayAndNewBoot(t *testing.T) {
	f := newFixture(t)
	first := f.send(f.provider(), f.cr.GetName())
	if repeated := f.send(f.provider(), f.cr.GetName()); repeated != first {
		t.Fatalf("restart changed reference: %q != %q", repeated, first)
	}
	f.addCR("same-boot")
	if adopted := f.send(f.provider(), "same-boot"); adopted != first {
		t.Fatalf("new CR on same boot did not adopt original reference")
	}
	if f.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", f.calls.Load())
	}
	f.updateNode(func(node *corev1.Node) { node.Status.NodeInfo.BootID = "boot-b" })
	if replayed := f.send(f.provider(), "same-boot"); replayed != first || f.calls.Load() != 1 {
		t.Fatal("previously adopted CR initiated another reboot after boot changed")
	}
	f.addCR("new-boot")
	second := f.send(f.provider(), "new-boot")
	if second == first || f.calls.Load() != 2 {
		t.Fatalf("new CR on new ready boot: reference=%q calls=%d", second, f.calls.Load())
	}
	if replayed := f.send(f.provider(), f.cr.GetName()); replayed != first || f.calls.Load() != 2 {
		t.Fatal("old originating CR initiated another reboot")
	}
	cm, err := f.kube.CoreV1().ConfigMaps("provider-system").Get(context.Background(), journalName(f.node.UID), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	j, err := decodeJournal(cm)
	if err != nil || len(j.Operations) != 2 || len(j.Bindings) != 3 || len(cm.OwnerReferences) != 0 {
		t.Fatalf("journal must retain operations and bindings without CR owner: %v %+v", err, j)
	}
}

func TestAmbiguousRebootFailureNeverResubmits(t *testing.T) {
	f := newFixture(t)
	f.err = errors.New("timeout with private response and super-secret-credential")
	_, err := f.provider().SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: f.cr.GetName()})
	if status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("unsafe or incorrect RPC error: %v", err)
	}
	ref := f.send(f.provider(), f.cr.GetName())
	f.addCR("retry-after-timeout")
	if adopted := f.send(f.provider(), "retry-after-timeout"); adopted != ref || f.calls.Load() != 1 {
		t.Fatalf("ambiguous request was resubmitted: calls=%d", f.calls.Load())
	}
	f.updateNode(func(node *corev1.Node) { node.Status.NodeInfo.BootID = "boot-b" })
	if ready, err := f.ready(f.provider(), ref); err != nil || !ready {
		t.Fatalf("a successfully observed boot should resolve ambiguity: ready=%v err=%v", ready, err)
	}
}

func TestReservationBeforeSendSurvivesInterruptedProcess(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.kube.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cm := action.(ktesting.CreateAction).GetObject().(*corev1.ConfigMap)
		if err := f.kube.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, cm.Namespace); err != nil {
			return true, nil, err
		}
		cancel() // A reservation exists, but the process cannot dispatch its POST.
		return true, cm, nil
	})
	_, err := f.provider().SendRebootSignal(ctx, &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: f.cr.GetName()})
	if status.Code(err) != codes.Canceled || f.calls.Load() != 0 {
		t.Fatalf("expected interruption before send: err=%v calls=%d", err, f.calls.Load())
	}
	ref := f.send(f.provider(), f.cr.GetName())
	if ref == "" || f.calls.Load() != 0 {
		t.Fatal("restart incorrectly dispatched a previously reserved attempt")
	}
	if ready, err := f.ready(f.provider(), ref); err != nil || ready {
		t.Fatalf("an unchanged boot cannot resolve a reserved attempt: %v %v", ready, err)
	}
}

func TestRebootGuards(t *testing.T) {
	tests := map[string]func(*fixture){
		"uncordoned":      func(f *fixture) { f.node.Spec.Unschedulable = false },
		"missing boot":    func(f *fixture) { f.node.Status.NodeInfo.BootID = "" },
		"missing UID":     func(f *fixture) { f.node.UID = "" },
		"unmapped":        func(f *fixture) { f.node.Labels = nil },
		"invalid mapping": func(f *fixture) { f.node.Labels[DefaultNodeNameLabel] = "../another-machine" },
		"mismatched CR":   func(f *fixture) { _ = unstructured.SetNestedField(f.cr.Object, "different-node", "spec", "nodeName") },
		"deleted CR":      func(f *fixture) { now := metav1.Now(); f.cr.SetDeletionTimestamp(&now) },
		"deleted node":    func(f *fixture) { now := metav1.Now(); f.node.DeletionTimestamp = &now },
		"completed timestamp": func(f *fixture) {
			_ = unstructured.SetNestedField(f.cr.Object, "2026-01-01T00:00:00Z", "status", "completionTime")
		},
		"completed condition": func(f *fixture) {
			_ = unstructured.SetNestedSlice(f.cr.Object, []interface{}{map[string]interface{}{"type": "NodeReady", "status": "True"}}, "status", "conditions")
		},
		"replaced node": func(f *fixture) { f.cr.SetAnnotations(map[string]string{NodeUIDAnnotation: "old-node-uid"}) },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			change(f)
			f.kube = fake.NewSimpleClientset(f.node)
			f.dynamic = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), f.cr)
			_, err := f.provider().SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: f.cr.GetName()})
			if status.Code(err) != codes.FailedPrecondition || f.calls.Load() != 0 {
				t.Fatalf("guard failed: error=%v calls=%d", err, f.calls.Load())
			}
		})
	}
}

func TestIsNodeReadyRequiresSameUIDNewBootAndReadyCondition(t *testing.T) {
	f := newFixture(t)
	ref := f.send(f.provider(), f.cr.GetName())
	tests := []struct {
		name      string
		boot      string
		condition corev1.ConditionStatus
		want      bool
	}{
		{"initially ready is insufficient", "boot-a", corev1.ConditionTrue, false},
		{"unknown boot", "", corev1.ConditionTrue, false},
		{"new boot not ready", "boot-b", corev1.ConditionFalse, false},
		{"new boot unknown readiness", "boot-b", corev1.ConditionUnknown, false},
		{"new boot ready", "boot-b", corev1.ConditionTrue, true},
		{"readiness can fall again", "boot-b", corev1.ConditionFalse, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.updateNode(func(node *corev1.Node) {
				node.Status.NodeInfo.BootID = tt.boot
				node.Status.Conditions[0].Status = tt.condition
			})
			if ready, err := f.ready(f.provider(), ref); err != nil || ready != tt.want {
				t.Fatalf("ready=%v want=%v err=%v", ready, tt.want, err)
			}
		})
	}
	f.updateNode(func(node *corev1.Node) { node.UID = "replacement-node-uid" })
	if _, err := f.ready(f.provider(), ref); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("replacement must not complete original reboot: %v", err)
	}
	_, err := f.provider().SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: f.cr.GetName()})
	if status.Code(err) != codes.FailedPrecondition || f.calls.Load() != 1 {
		t.Fatalf("old CR must not reboot replacement node: error=%v calls=%d", err, f.calls.Load())
	}
}

func TestMappingIsPinnedByJournal(t *testing.T) {
	f := newFixture(t)
	ref := f.send(f.provider(), f.cr.GetName())
	f.updateNode(func(node *corev1.Node) { node.Labels[DefaultNodeNameLabel] = "different-compute-machine" })
	f.addCR("changed-mapping")
	_, err := f.provider().SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: "changed-mapping"})
	if status.Code(err) != codes.FailedPrecondition || f.calls.Load() != 1 {
		t.Fatalf("mapping change bypassed guard: error=%v calls=%d", err, f.calls.Load())
	}
	if _, err := f.ready(f.provider(), ref); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("mapping change bypassed readiness guard: %v", err)
	}
}

func TestPersistenceFailurePreventsDispatch(t *testing.T) {
	f := newFixture(t)
	f.kube.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("configmaps"), "journal", errors.New("secret database message"))
	})
	_, err := f.provider().SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: f.cr.GetName()})
	if status.Code(err) != codes.PermissionDenied || strings.Contains(err.Error(), "secret") || f.calls.Load() != 0 {
		t.Fatalf("failed persistence allowed dispatch or leaked details: %v calls=%d", err, f.calls.Load())
	}
}

func TestAmbiguousPersistenceFailureRetainsReservation(t *testing.T) {
	f := newFixture(t)
	f.kube.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cm := action.(ktesting.CreateAction).GetObject().(*corev1.ConfigMap)
		if err := f.kube.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, cm.Namespace); err != nil {
			return true, nil, err
		}
		return true, nil, context.DeadlineExceeded
	})
	_, err := f.provider().SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: f.cr.GetName()})
	if status.Code(err) != codes.DeadlineExceeded || f.calls.Load() != 0 {
		t.Fatalf("ambiguous persistence must not dispatch: %v calls=%d", err, f.calls.Load())
	}
	f.send(f.provider(), f.cr.GetName())
	if f.calls.Load() != 0 {
		t.Fatal("restart dispatched a reservation whose persistence timed out")
	}
}

func TestScopeDriftFailsClosed(t *testing.T) {
	f := newFixture(t)
	ref := f.send(f.provider(), f.cr.GetName())
	f.addCR("different-scope")
	p := f.provider()
	p.config.Scope = "another-project-or-region"
	_, err := p.SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: "different-scope"})
	if status.Code(err) != codes.FailedPrecondition || f.calls.Load() != 1 {
		t.Fatalf("scope drift allowed dispatch: %v calls=%d", err, f.calls.Load())
	}
	if _, err := f.ready(p, ref); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("scope drift allowed readiness: %v", err)
	}
}

func TestNewBootMustBeReadyBeforeNewReboot(t *testing.T) {
	f := newFixture(t)
	f.send(f.provider(), f.cr.GetName())
	f.updateNode(func(node *corev1.Node) {
		node.Status.NodeInfo.BootID = "boot-b"
		node.Status.Conditions[0].Status = corev1.ConditionFalse
	})
	f.addCR("premature-new-reboot")
	_, err := f.provider().SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: "premature-new-reboot"})
	if status.Code(err) != codes.FailedPrecondition || f.calls.Load() != 1 {
		t.Fatalf("new boot without readiness allowed another reboot: %v calls=%d", err, f.calls.Load())
	}
}

func TestCRReplacementBetweenReservationAndDispatch(t *testing.T) {
	f := newFixture(t)
	f.kube.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		cm := action.(ktesting.CreateAction).GetObject().(*corev1.ConfigMap)
		if err := f.kube.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, cm.Namespace); err != nil {
			return true, nil, err
		}
		if err := f.dynamic.Tracker().Delete(rebootNodes, "", f.cr.GetName()); err != nil {
			return true, nil, err
		}
		err := f.dynamic.Tracker().Create(rebootNodes, testCR(f.cr.GetName(), "replacement-cr-uid", f.node.Name), "")
		return true, cm, err
	})
	_, err := f.provider().SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: f.cr.GetName()})
	if status.Code(err) != codes.FailedPrecondition || f.calls.Load() != 0 {
		t.Fatalf("deleted originating CR allowed dispatch: %v calls=%d", err, f.calls.Load())
	}
	f.send(f.provider(), f.cr.GetName())
	if f.calls.Load() != 0 {
		t.Fatal("replacement CR on same boot bypassed durable reservation")
	}
}

func TestInvalidJournalFailsClosed(t *testing.T) {
	f := newFixture(t)
	_, err := f.kube.CoreV1().ConfigMaps("provider-system").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: journalName(f.node.UID), Namespace: "provider-system"},
		Data:       map[string]string{journalKey: "{}"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.provider().SendRebootSignal(context.Background(), &cspv1alpha1.SendRebootSignalRequest{NodeName: f.node.Name, CrName: f.cr.GetName()})
	if status.Code(err) != codes.FailedPrecondition || f.calls.Load() != 0 {
		t.Fatalf("invalid journal allowed dispatch: %v calls=%d", err, f.calls.Load())
	}
}

func TestMalformedRequestsAndTermination(t *testing.T) {
	f := newFixture(t)
	p := f.provider()
	for _, req := range []*cspv1alpha1.SendRebootSignalRequest{nil, {}, {NodeName: "node", CrName: "../../cr"}, {NodeName: "bad node", CrName: "cr"}} {
		if _, err := p.SendRebootSignal(context.Background(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("malformed request accepted: %v %v", req, err)
		}
	}
	for _, ref := range []string{"", "../../secret", "v1.a.b", strings.Repeat("f", 500)} {
		if _, err := f.ready(p, ref); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("malformed reference accepted: %q %v", ref, err)
		}
	}
	ref := f.send(p, f.cr.GetName())
	if _, err := p.IsNodeReady(context.Background(), &cspv1alpha1.IsNodeReadyRequest{NodeName: "different-node", RequestId: ref}); status.Code(err) != codes.NotFound {
		t.Fatalf("reference accepted for wrong node: %v", err)
	}
	if _, err := p.SendTerminateSignal(context.Background(), &cspv1alpha1.SendTerminateSignalRequest{NodeName: f.node.Name}); status.Code(err) != codes.Unimplemented || f.calls.Load() != 1 {
		t.Fatalf("termination must remain unimplemented: %v calls=%d", err, f.calls.Load())
	}
}
