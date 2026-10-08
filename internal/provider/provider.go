// Package provider adapts NVSentinel's reboot RPCs to a non-idempotent Compute
// API. A durable reservation precedes every outbound request. A crash in that
// interval can require manual recovery; it must never cause an automatic retry.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	cspv1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/csp/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	DefaultNodeNameLabel = "nvsentinel.mistral.ai.node-restriction.kubernetes.io/node-name"
	// NodeUIDAnnotation pins a CR to the Kubernetes Node incarnation observed
	// before any reboot, including across crashes and same-name replacements.
	NodeUIDAnnotation = "nvsentinel.mistral.ai/node-uid"
)

var rebootNodes = schema.GroupVersionResource{
	Group: "janitor.dgxc.nvidia.com", Version: "v1alpha1", Resource: "rebootnodes",
}

// Rebooter accepts a provider machine name explicitly mapped by a Node label.
// Prepare resolves credentials without rebooting. Reboot must perform at most
// one reboot HTTP request and must never retry that request.
type Rebooter interface {
	Prepare(context.Context) error
	Reboot(context.Context, string) error
}

type Config struct {
	Namespace     string
	NodeNameLabel string
	// Scope identifies the immutable Compute endpoint, project, and region.
	// Use a stable hash of those non-secret fields; never include credentials.
	Scope string
}

type Provider struct {
	cspv1alpha1.UnimplementedCSPProviderServiceServer
	config   Config
	kube     kubernetes.Interface
	dynamic  dynamic.Interface
	rebooter Rebooter
}

func New(config Config, kube kubernetes.Interface, dynamicClient dynamic.Interface, rebooter Rebooter) (*Provider, error) {
	if len(validation.IsDNS1123Label(config.Namespace)) != 0 {
		return nil, fmt.Errorf("a valid journal namespace is required")
	}
	if config.NodeNameLabel == "" {
		config.NodeNameLabel = DefaultNodeNameLabel
	}
	if len(validation.IsQualifiedName(config.NodeNameLabel)) != 0 {
		return nil, fmt.Errorf("invalid node-name label key")
	}
	// Match NodeRestriction's protected label namespaces exactly. An ordinary
	// custom label can be rewritten by a kubelet to target unrelated hardware.
	namespace, _, qualified := strings.Cut(config.NodeNameLabel, "/")
	if !qualified || (namespace != corev1.LabelNamespaceNodeRestriction &&
		!strings.HasSuffix(namespace, "."+corev1.LabelNamespaceNodeRestriction)) {
		return nil, fmt.Errorf("node-name label must use a protected node-restriction.kubernetes.io namespace")
	}
	if config.Scope == "" || len(config.Scope) > 256 {
		return nil, fmt.Errorf("a stable provider scope identifier is required (at most 256 bytes)")
	}
	if kube == nil || dynamicClient == nil || rebooter == nil {
		return nil, fmt.Errorf("Kubernetes, dynamic, and reboot clients are required")
	}
	return &Provider{config: config, kube: kube, dynamic: dynamicClient, rebooter: rebooter}, nil
}

func (p *Provider) SendRebootSignal(ctx context.Context, req *cspv1alpha1.SendRebootSignalRequest) (*cspv1alpha1.SendRebootSignalResponse, error) {
	if req == nil || !validName(req.NodeName) || !validName(req.CrName) {
		return nil, status.Error(codes.InvalidArgument, "valid node_name and cr_name are required")
	}
	var ref string
	var reservedNode *corev1.Node
	var reservedTarget string
	var reservedCRUID types.UID
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Refresh all guards on every CAS attempt. Neither informers nor a prior
		// reconcile's cached observations authorize a destructive operation.
		cr, node, target, err := p.authorize(ctx, req.NodeName, req.CrName)
		if err != nil {
			return err
		}
		if err := p.pinCR(ctx, cr, node.UID); err != nil {
			return err
		}
		cm, err := p.kube.CoreV1().ConfigMaps(p.config.Namespace).Get(ctx, journalName(node.UID), metav1.GetOptions{})
		var j *journal
		if apierrors.IsNotFound(err) {
			cm, j = nil, newJournal(node, target, p.config.Scope)
		} else if err != nil {
			return err
		} else {
			j, err = decodeJournal(cm)
			if err != nil {
				return status.Error(codes.FailedPrecondition, "reboot journal is invalid; operator intervention required")
			}
			if j.NodeUID != node.UID || j.NodeName != node.Name || j.Target != target || j.Scope != p.config.Scope {
				return status.Error(codes.FailedPrecondition, "node identity, provider scope or mapping differs from the reboot journal")
			}
		}
		if previous, ok := j.Bindings[cr.GetUID()]; ok {
			ref = previous
			return nil
		}
		// Bind every newly observed CR, even when it adopts an existing attempt.
		// Otherwise that CR could become an accidental new request after boot.
		for previous, op := range j.Operations {
			if op.BootID == node.Status.NodeInfo.BootID {
				j.Bindings[cr.GetUID()] = previous
				if err := p.saveJournal(ctx, j, cm); err != nil {
					return err
				}
				ref = previous
				return nil
			}
		}
		if current := j.Operations[j.Current]; current != nil {
			if !nodeReady(node) {
				return status.Error(codes.FailedPrecondition, "the previous reboot has not reached a new Ready boot")
			}
			current.Phase = phaseComplete
		}
		// Credential exchange is safe to retry before reserving a destructive
		// operation. Replays bypass it so an identity outage cannot hide an
		// existing reservation or prevent observation of reboot completion.
		if err := p.rebooter.Prepare(ctx); err != nil {
			return status.Error(codes.Unavailable, "Compute credentials could not be prepared; this call did not reserve a reboot")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		candidate, err := newReference(node.UID)
		if err != nil {
			return status.Error(codes.Internal, "could not allocate reboot reference")
		}
		j.Operations[candidate] = &operation{
			Reference: candidate, CRUID: cr.GetUID(), CRName: cr.GetName(),
			BootID: node.Status.NodeInfo.BootID, CreatedAt: time.Now().UTC(), Phase: phaseReserved,
		}
		j.Bindings[cr.GetUID()], j.Current = candidate, candidate
		if err := p.saveJournal(ctx, j, cm); err != nil {
			return err
		}
		ref, reservedNode, reservedTarget, reservedCRUID = candidate, node, target, cr.GetUID()
		return nil
	})
	if err != nil {
		return nil, rpcError(err)
	}
	if reservedNode == nil {
		slog.DebugContext(ctx, "reusing durable reboot reservation", "node", req.NodeName, "cr", req.CrName, "request_id", ref)
		return &cspv1alpha1.SendRebootSignalResponse{RequestId: ref}, nil
	}
	slog.InfoContext(ctx, "reboot reserved before submission", "node", req.NodeName, "cr", req.CrName, "request_id", ref)
	// The reservation remains even if a guard changes before the HTTP call. A
	// retry cannot assume that a previous process stopped at the same point.
	liveCR, liveNode, target, err := p.authorize(ctx, req.NodeName, req.CrName)
	if err != nil {
		slog.WarnContext(ctx, "reboot guard failed after reservation; reservation retained", "node", req.NodeName, "request_id", ref, "code", status.Code(rpcError(err)).String())
		return nil, rpcError(err)
	}
	if liveCR.GetUID() != reservedCRUID || liveCR.GetAnnotations()[NodeUIDAnnotation] != string(reservedNode.UID) ||
		liveNode.UID != reservedNode.UID || liveNode.Status.NodeInfo.BootID != reservedNode.Status.NodeInfo.BootID || target != reservedTarget {
		slog.WarnContext(ctx, "node or RebootNode changed after reservation; submission suppressed", "node", req.NodeName, "request_id", ref)
		return nil, status.Error(codes.FailedPrecondition, "node changed after reboot reservation; operator intervention required")
	}
	if err := ctx.Err(); err != nil {
		return nil, rpcError(err)
	}
	err = p.rebooter.Reboot(ctx, reservedTarget)
	phase := phaseSubmitted
	if err != nil {
		phase = phaseUnconfirmed
	}
	// The durable reservation already provides safety. Failure to record the
	// outcome must never clear it or cause a second outbound request.
	if recordErr := p.recordPhase(ctx, reservedNode.UID, ref, phase); recordErr != nil {
		slog.WarnContext(ctx, "could not record reboot outcome; durable reservation retained", "node", req.NodeName, "request_id", ref, "phase", phase, "code", status.Code(rpcError(recordErr)).String())
	}
	if err != nil {
		slog.WarnContext(ctx, "reboot outcome unconfirmed; automatic resubmission suppressed", "node", req.NodeName, "request_id", ref)
		return nil, status.Error(codes.Unavailable, "reboot outcome is unconfirmed; automatic resubmission is suppressed")
	}
	slog.InfoContext(ctx, "reboot accepted; awaiting a new Ready boot", "node", req.NodeName, "request_id", ref)
	return &cspv1alpha1.SendRebootSignalResponse{RequestId: ref}, nil
}

func (p *Provider) IsNodeReady(ctx context.Context, req *cspv1alpha1.IsNodeReadyRequest) (*cspv1alpha1.IsNodeReadyResponse, error) {
	if req == nil || !validName(req.NodeName) {
		return nil, status.Error(codes.InvalidArgument, "a valid node_name is required")
	}
	parts := referencePattern.FindStringSubmatch(req.RequestId)
	if len(parts) != 3 {
		return nil, status.Error(codes.InvalidArgument, "invalid reboot reference")
	}
	cm, err := p.kube.CoreV1().ConfigMaps(p.config.Namespace).Get(ctx, journalPrefix+parts[1], metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, status.Error(codes.NotFound, "reboot reference does not exist")
	}
	if err != nil {
		return nil, rpcError(err)
	}
	j, err := decodeJournal(cm)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "reboot journal is invalid; operator intervention required")
	}
	op := j.Operations[req.RequestId]
	if op == nil || j.NodeName != req.NodeName {
		return nil, status.Error(codes.NotFound, "reboot reference does not match the node")
	}
	node, err := p.kube.CoreV1().Nodes().Get(ctx, req.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, rpcError(err)
	}
	if node.UID != j.NodeUID || node.DeletionTimestamp != nil {
		return nil, status.Error(codes.FailedPrecondition, "the original node was deleted or replaced")
	}
	if node.Labels[p.config.NodeNameLabel] != j.Target || j.Scope != p.config.Scope {
		return nil, status.Error(codes.FailedPrecondition, "provider scope or mapping differs from the reboot journal")
	}
	ready := node.Status.NodeInfo.BootID != "" && node.Status.NodeInfo.BootID != op.BootID && nodeReady(node)
	if ready {
		if err := p.recordPhase(ctx, node.UID, req.RequestId, phaseComplete); err != nil {
			return nil, rpcError(err)
		}
	}
	return &cspv1alpha1.IsNodeReadyResponse{IsReady: ready}, nil
}

func (p *Provider) authorize(ctx context.Context, nodeName, crName string) (*unstructured.Unstructured, *corev1.Node, string, error) {
	cr, err := p.dynamic.Resource(rebootNodes).Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, "", err
	}
	crNode, _, err := unstructured.NestedString(cr.Object, "spec", "nodeName")
	if err != nil || cr.GetUID() == "" || crNode != nodeName || cr.GetDeletionTimestamp() != nil {
		return nil, nil, "", status.Error(codes.FailedPrecondition, "RebootNode must be live and refer to the requested node")
	}
	if _, exists, err := unstructured.NestedFieldNoCopy(cr.Object, "status", "completionTime"); err != nil || exists {
		return nil, nil, "", status.Error(codes.FailedPrecondition, "RebootNode is completed or has invalid status")
	}
	conditions, _, err := unstructured.NestedSlice(cr.Object, "status", "conditions")
	if err != nil {
		return nil, nil, "", status.Error(codes.FailedPrecondition, "RebootNode has invalid status")
	}
	for _, item := range conditions {
		condition, ok := item.(map[string]interface{})
		if !ok || (condition["type"] == "NodeReady" && condition["status"] == "True") {
			return nil, nil, "", status.Error(codes.FailedPrecondition, "RebootNode is completed or has invalid status")
		}
	}
	node, err := p.kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, "", err
	}
	target := node.Labels[p.config.NodeNameLabel]
	if node.UID == "" || node.Status.NodeInfo.BootID == "" || node.DeletionTimestamp != nil || !node.Spec.Unschedulable {
		return nil, nil, "", status.Error(codes.FailedPrecondition, "node must be live, cordoned, and have a UID and boot ID")
	}
	if !validName(target) {
		return nil, nil, "", status.Error(codes.FailedPrecondition, "node requires a valid explicit Compute node-name label")
	}
	if pinned := cr.GetAnnotations()[NodeUIDAnnotation]; pinned != "" && pinned != string(node.UID) {
		return nil, nil, "", status.Error(codes.FailedPrecondition, "RebootNode belongs to a different node incarnation")
	}
	return cr, node, target, nil
}

func (p *Provider) pinCR(ctx context.Context, cr *unstructured.Unstructured, uid types.UID) error {
	if cr.GetAnnotations()[NodeUIDAnnotation] == string(uid) {
		return nil
	}
	patch, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{
		"uid": cr.GetUID(), "resourceVersion": cr.GetResourceVersion(),
		"annotations": map[string]string{NodeUIDAnnotation: string(uid)},
	}})
	if err != nil {
		return err
	}
	_, err = p.dynamic.Resource(rebootNodes).Patch(ctx, cr.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (p *Provider) saveJournal(ctx context.Context, j *journal, previous *corev1.ConfigMap) error {
	cm, err := encodeJournal(j, previous, p.config.Namespace)
	if err != nil {
		return status.Error(codes.ResourceExhausted, "reboot journal cannot accept more records; operator intervention required")
	}
	if previous == nil {
		_, err = p.kube.CoreV1().ConfigMaps(p.config.Namespace).Create(ctx, cm, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return apierrors.NewConflict(corev1.Resource("configmaps"), cm.Name, err)
		}
	} else {
		_, err = p.kube.CoreV1().ConfigMaps(p.config.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

func (p *Provider) recordPhase(ctx context.Context, uid types.UID, ref, phase string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := p.kube.CoreV1().ConfigMaps(p.config.Namespace).Get(ctx, journalName(uid), metav1.GetOptions{})
		if err != nil {
			return err
		}
		j, err := decodeJournal(cm)
		if err != nil || j.Operations[ref] == nil {
			return status.Error(codes.FailedPrecondition, "reboot journal is invalid; operator intervention required")
		}
		op := j.Operations[ref]
		if op.Phase == phaseComplete || op.Phase == phase {
			return nil
		}
		op.Phase = phase
		if err := p.saveJournal(ctx, j, cm); err != nil {
			return err
		}
		if phase == phaseComplete {
			slog.InfoContext(ctx, "reboot completion observed", "node", j.NodeName, "request_id", ref)
		}
		return nil
	})
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func validName(name string) bool { return len(validation.IsDNS1123Subdomain(name)) == 0 }

// Client errors may contain HTTP bodies, URLs or authorization details. Only
// explicit provider status errors and sanitized categories cross the RPC API.
func rpcError(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return status.FromContextError(err).Err()
	}
	if apierrors.IsNotFound(err) {
		return status.Error(codes.NotFound, "required Kubernetes resource does not exist")
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return status.Error(codes.PermissionDenied, "Kubernetes access denied")
	}
	return status.Error(codes.Unavailable, "Kubernetes operation failed; retry may adopt an existing reboot reservation")
}
