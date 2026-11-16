package kube

import (
	"context"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TriggerCronJob creates a Job from the CronJob's template now (like kubectl create job --from=cronjob/x).
func (c *Client) TriggerCronJob(ctx context.Context, ns, name string) (string, error) {
	cj, err := c.Clientset.BatchV1().CronJobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	jobName := fmt.Sprintf("%s-manual-%d", name, time.Now().Unix()%1000000)
	if len(jobName) > 63 {
		jobName = jobName[len(jobName)-63:]
	}
	yes := true
	ann := map[string]string{"cronjob.kubernetes.io/instantiate": "manual"}
	for k, v := range cj.Spec.JobTemplate.Annotations {
		ann[k] = v
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: cj.Spec.JobTemplate.Labels, Annotations: ann,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "CronJob", Name: cj.Name, UID: cj.UID, Controller: &yes}}},
		Spec: cj.Spec.JobTemplate.Spec,
	}
	_, err = c.Clientset.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
	return jobName, err
}

// SuspendCronJob suspends (true) or resumes (false) a CronJob.
func (c *Client) SuspendCronJob(ctx context.Context, ns, name string, suspend bool) error {
	patch := fmt.Sprintf(`{"spec":{"suspend":%t}}`, suspend)
	_, err := c.Clientset.BatchV1().CronJobs(ns).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// JobRun is one run of a CronJob.
type JobRun struct {
	Name            string
	Start, Complete time.Time
	Status          string
	Health          Health
	Manual          bool
}

// Duration of the run so far (or total, when finished).
func (r JobRun) Duration() time.Duration {
	if r.Start.IsZero() {
		return 0
	}
	end := r.Complete
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(r.Start).Round(time.Second)
}

// CronJobRuns returns the Jobs a CronJob created, newest first, and whether it is suspended.
func (c *Client) CronJobRuns(ctx context.Context, ns, name string) ([]JobRun, bool, error) {
	cj, err := c.Clientset.BatchV1().CronJobs(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, false, err
	}
	jobs, err := c.Clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, false, err
	}
	var out []JobRun
	for _, j := range jobs.Items {
		owned := false
		for _, o := range j.OwnerReferences {
			if o.Kind == "CronJob" && o.UID == cj.UID {
				owned = true
			}
		}
		if !owned {
			continue
		}
		r := JobRun{Name: j.Name, Manual: j.Annotations["cronjob.kubernetes.io/instantiate"] == "manual"}
		if j.Status.StartTime != nil {
			r.Start = j.Status.StartTime.Time
		} else {
			r.Start = j.CreationTimestamp.Time
		}
		if j.Status.CompletionTime != nil {
			r.Complete = j.Status.CompletionTime.Time
		}
		switch {
		case j.Status.Succeeded > 0:
			r.Status, r.Health = "Succeeded", HealthOK
		case j.Status.Failed > 0 && j.Status.Active == 0:
			r.Status, r.Health = "Failed", HealthErr
			for _, cond := range j.Status.Conditions {
				if cond.Type == batchv1.JobFailed && cond.Reason != "" {
					r.Status += " (" + cond.Reason + ")"
					if !cond.LastTransitionTime.IsZero() {
						r.Complete = cond.LastTransitionTime.Time
					}
				}
			}
		case j.Status.Active > 0:
			r.Status, r.Health = "Running", HealthWarn
		default:
			r.Status, r.Health = "Pending", HealthNeutral
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.After(out[j].Start) })
	return out, cj.Spec.Suspend != nil && *cj.Spec.Suspend, nil
}
