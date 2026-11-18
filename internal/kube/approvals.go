package kube

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Approval is a request for a second person to approve a change. It is stored as a ConfigMap in the
// approvals namespace, so whoever can read that namespace sees it from their own kubeweave.
type Approval struct {
	ID, Context, Namespace, Target, Action string
	Requester, Approver, Reason, Status    string // Status: pending, approved, rejected, expired
	Created, Decided                       time.Time
}

const approvalLabel = "kubeweave.io/approval"

// WhoAmI is the user name the API server sees (SelfSubjectReview), falling back to the kubeconfig user.
func (c *Client) WhoAmI(ctx context.Context) string {
	if r, err := c.Clientset.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{}); err == nil && r.Status.UserInfo.Username != "" {
		return r.Status.UserInfo.Username
	}
	if kc, ok := c.raw.Contexts[c.Context]; ok && kc.AuthInfo != "" {
		return kc.AuthInfo
	}
	return "unknown"
}

func approvalFrom(cm *corev1.ConfigMap, expire time.Duration) Approval {
	d := cm.Data
	a := Approval{ID: cm.Name, Context: d["context"], Namespace: d["namespace"], Target: d["target"], Action: d["action"],
		Requester: d["requester"], Approver: d["approver"], Reason: d["reason"], Status: d["status"], Created: cm.CreationTimestamp.Time}
	a.Decided, _ = time.Parse(time.RFC3339, d["decided"])
	if t, err := time.Parse(time.RFC3339, d["created"]); err == nil {
		a.Created = t
	}
	if a.Status == "pending" && expire > 0 && time.Since(a.Created) > expire {
		a.Status = "expired"
	}
	return a
}

// RequestApproval stores a pending approval and returns it.
func (c *Client) RequestApproval(ctx context.Context, approvalsNS string, a Approval) (Approval, error) {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	a.ID = "kubeweave-approval-" + hex.EncodeToString(b)
	a.Status = "pending"
	if a.Requester == "" {
		a.Requester = c.WhoAmI(ctx)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: a.ID, Namespace: approvalsNS,
		Labels: map[string]string{approvalLabel: "true", "app.kubernetes.io/managed-by": "kubeweave"}},
		Data: map[string]string{"context": a.Context, "namespace": a.Namespace, "target": a.Target, "action": a.Action,
			"requester": a.Requester, "reason": a.Reason, "status": "pending", "created": time.Now().UTC().Format(time.RFC3339)}}
	created, err := c.Clientset.CoreV1().ConfigMaps(approvalsNS).Create(ctx, cm, metav1.CreateOptions{FieldManager: "kubeweave"})
	if apierrors.IsNotFound(err) {
		return a, fmt.Errorf("namespace %s does not exist: create it (kubectl create namespace %s) and let your team read/write ConfigMaps there", approvalsNS, approvalsNS)
	}
	if err != nil {
		return a, err
	}
	a.Created = approvalFrom(created, 0).Created
	return a, nil
}

// Approvals lists approval requests, newest first.
func (c *Client) Approvals(ctx context.Context, approvalsNS string, expire time.Duration) ([]Approval, error) {
	l, err := c.Clientset.CoreV1().ConfigMaps(approvalsNS).List(ctx, metav1.ListOptions{LabelSelector: approvalLabel + "=true"})
	if err != nil {
		return nil, err
	}
	out := make([]Approval, 0, len(l.Items))
	for i := range l.Items {
		out = append(out, approvalFrom(&l.Items[i], expire))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// DecideApproval approves or rejects a pending request. The requester can never approve their own request.
func (c *Client) DecideApproval(ctx context.Context, approvalsNS, id string, approve bool, expire time.Duration) (Approval, error) {
	cm, err := c.Clientset.CoreV1().ConfigMaps(approvalsNS).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return Approval{}, err
	}
	a := approvalFrom(cm, expire)
	if a.Status != "pending" {
		return a, fmt.Errorf("request is %s, not pending", a.Status)
	}
	me := c.WhoAmI(ctx)
	if me == a.Requester {
		return a, fmt.Errorf("you (%s) asked for this change; someone else has to approve it", me)
	}
	a.Status, a.Approver, a.Decided = "rejected", me, time.Now().UTC()
	if approve {
		a.Status = "approved"
	}
	cm.Data["status"], cm.Data["approver"], cm.Data["decided"] = a.Status, me, a.Decided.Format(time.RFC3339)
	// Update carries the resourceVersion, so two people deciding at once cannot both win.
	if _, err := c.Clientset.CoreV1().ConfigMaps(approvalsNS).Update(ctx, cm, metav1.UpdateOptions{FieldManager: "kubeweave"}); err != nil {
		return a, err
	}
	return a, nil
}

// CloseApproval marks an approved request as used, so it cannot run twice.
func (c *Client) CloseApproval(ctx context.Context, approvalsNS, id, result string) error {
	cm, err := c.Clientset.CoreV1().ConfigMaps(approvalsNS).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return err
	}
	cm.Data["status"], cm.Data["result"] = "done", result
	_, err = c.Clientset.CoreV1().ConfigMaps(approvalsNS).Update(ctx, cm, metav1.UpdateOptions{FieldManager: "kubeweave"})
	return err
}
