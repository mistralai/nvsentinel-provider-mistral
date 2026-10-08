package provider

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	journalPrefix = "nvsentinel-reboot-"
	journalKey    = "journal.json"
	// Keep well below the API's 1 MiB limit, including object metadata. Retaining
	// history is essential: a previously handled CR must never start a new reboot.
	maxJournalBytes  = 768 * 1024
	phaseReserved    = "reserved"
	phaseSubmitted   = "submitted"
	phaseUnconfirmed = "unconfirmed"
	phaseComplete    = "complete"
)

var referencePattern = regexp.MustCompile(`^v1\.([a-f0-9]{32})\.([a-f0-9]{32})$`)

type journal struct {
	Version    int                   `json:"version"`
	NodeUID    types.UID             `json:"nodeUID"`
	NodeName   string                `json:"nodeName"`
	Target     string                `json:"target"`
	Scope      string                `json:"scope"`
	Current    string                `json:"current"`
	Operations map[string]*operation `json:"operations"`
	Bindings   map[types.UID]string  `json:"bindings"`
}

type operation struct {
	Reference string    `json:"reference"`
	CRUID     types.UID `json:"crUID"`
	CRName    string    `json:"crName"`
	BootID    string    `json:"bootID"`
	CreatedAt time.Time `json:"createdAt"`
	Phase     string    `json:"phase"`
}

func nodeHash(uid types.UID) string {
	digest := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(digest[:16])
}

func journalName(uid types.UID) string { return journalPrefix + nodeHash(uid) }

func newReference(uid types.UID) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "v1." + nodeHash(uid) + "." + hex.EncodeToString(random[:]), nil
}

func newJournal(node *corev1.Node, target, scope string) *journal {
	return &journal{
		Version: 1, NodeUID: node.UID, NodeName: node.Name, Target: target, Scope: scope,
		Operations: make(map[string]*operation), Bindings: make(map[types.UID]string),
	}
}

// decodeJournal rejects inconsistent state rather than treating it as an empty
// journal. Losing a reservation would allow a duplicate non-idempotent request.
func decodeJournal(cm *corev1.ConfigMap) (*journal, error) {
	var j journal
	if err := json.Unmarshal([]byte(cm.Data[journalKey]), &j); err != nil {
		return nil, fmt.Errorf("invalid reboot journal")
	}
	if j.Version != 1 || j.NodeUID == "" || j.NodeName == "" || j.Target == "" || j.Scope == "" ||
		cm.Name != journalName(j.NodeUID) || j.Current == "" ||
		len(j.Operations) == 0 || len(j.Bindings) == 0 || j.Operations[j.Current] == nil {
		return nil, fmt.Errorf("inconsistent reboot journal")
	}
	boots := make(map[string]bool)
	for ref, op := range j.Operations {
		parts := referencePattern.FindStringSubmatch(ref)
		if op == nil || len(parts) != 3 || parts[1] != nodeHash(j.NodeUID) ||
			op.Reference != ref || op.CRUID == "" || op.CRName == "" || op.BootID == "" ||
			op.CreatedAt.IsZero() || j.Bindings[op.CRUID] != ref || boots[op.BootID] {
			return nil, fmt.Errorf("inconsistent reboot operation")
		}
		switch op.Phase {
		case phaseReserved, phaseSubmitted, phaseUnconfirmed, phaseComplete:
		default:
			return nil, fmt.Errorf("invalid reboot phase")
		}
		boots[op.BootID] = true
	}
	for uid, ref := range j.Bindings {
		if uid == "" || j.Operations[ref] == nil {
			return nil, fmt.Errorf("invalid reboot binding")
		}
	}
	return &j, nil
}

func encodeJournal(j *journal, cm *corev1.ConfigMap, namespace string) (*corev1.ConfigMap, error) {
	payload, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxJournalBytes {
		return nil, fmt.Errorf("reboot journal capacity exhausted; operator intervention required")
	}
	if cm == nil {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: journalName(j.NodeUID), Namespace: namespace,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "nvsentinel-provider-mistral"},
		}}
	} else {
		cm = cm.DeepCopy()
	}
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[journalKey] = string(payload)
	return cm, nil
}
