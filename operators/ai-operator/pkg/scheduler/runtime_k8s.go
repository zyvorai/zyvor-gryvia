package scheduler

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ApplyRuntimePlan mutates one container. DRA plans only add annotations because
// ResourceClaimTemplate/DeviceClaim creation belongs to the controller and varies
// by installed DRA driver.
func ApplyRuntimePlan(pod *corev1.Pod, container int, plan RuntimePlan) error {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	for k, v := range plan.Annotations {
		pod.Annotations[k] = v
	}
	if container < 0 || container >= len(pod.Spec.Containers) {
		return nil
	}
	c := &pod.Spec.Containers[container]
	for k, v := range plan.Env {
		found := false
		for i := range c.Env {
			if c.Env[i].Name == k {
				c.Env[i].Value = v
				found = true
				break
			}
		}
		if !found {
			c.Env = append(c.Env, corev1.EnvVar{Name: k, Value: v})
		}
	}
	if plan.ResourceName != "" && plan.ResourceQty > 0 {
		if c.Resources.Limits == nil {
			c.Resources.Limits = corev1.ResourceList{}
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		q := *resource.NewQuantity(plan.ResourceQty, resource.DecimalSI)
		name := corev1.ResourceName(plan.ResourceName)
		c.Resources.Limits[name] = q
		c.Resources.Requests[name] = q
	}
	return nil
}
