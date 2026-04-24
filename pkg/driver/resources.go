package driver

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

// volumeAttributes keys recognised by the driver to configure the FUSE
// daemon container resources (mount pod or injected sidecar). Must stay
// in sync with pkg/webhook: the injector reads the same keys.
const (
	VolumeAttrCPULimit      = "cpuLimit"
	VolumeAttrCPURequest    = "cpuRequest"
	VolumeAttrMemoryLimit   = "memoryLimit"
	VolumeAttrMemoryRequest = "memoryRequest"
)

// MountResources carries already-parsed resource overrides. A nil field
// means "not set by the user"; the caller's defaults win.
type MountResources struct {
	CPULimit      *resource.Quantity
	CPURequest    *resource.Quantity
	MemoryLimit   *resource.Quantity
	MemoryRequest *resource.Quantity
}

// ParseMountResources reads the four resource keys from volumeAttributes.
// Invalid quantities are logged and dropped so a typo never blocks a pod.
func ParseMountResources(attrs map[string]string) MountResources {
	return MountResources{
		CPULimit:      parseQuantityAttr(VolumeAttrCPULimit, attrs[VolumeAttrCPULimit]),
		CPURequest:    parseQuantityAttr(VolumeAttrCPURequest, attrs[VolumeAttrCPURequest]),
		MemoryLimit:   parseQuantityAttr(VolumeAttrMemoryLimit, attrs[VolumeAttrMemoryLimit]),
		MemoryRequest: parseQuantityAttr(VolumeAttrMemoryRequest, attrs[VolumeAttrMemoryRequest]),
	}
}

func parseQuantityAttr(field, raw string) *resource.Quantity {
	if raw == "" {
		return nil
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		klog.Warningf("ignoring invalid %s=%q in volumeAttributes: %v", field, raw, err)
		return nil
	}
	return &q
}

// BuildResourceRequirements merges user overrides on top of the given
// defaults. Defaults apply only to requests (limits stay unset unless the
// user provides one). If a user override leaves request > limit for a
// resource, the request is clamped down to the limit so the apiserver
// never rejects the pod because of a conflicting hint.
func BuildResourceRequirements(overrides MountResources, defaultCPURequest, defaultMemoryRequest resource.Quantity) corev1.ResourceRequirements {
	req := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    defaultCPURequest,
			corev1.ResourceMemory: defaultMemoryRequest,
		},
	}

	if overrides.CPURequest != nil {
		req.Requests[corev1.ResourceCPU] = *overrides.CPURequest
	}
	if overrides.MemoryRequest != nil {
		req.Requests[corev1.ResourceMemory] = *overrides.MemoryRequest
	}
	if overrides.CPULimit != nil {
		if req.Limits == nil {
			req.Limits = corev1.ResourceList{}
		}
		req.Limits[corev1.ResourceCPU] = *overrides.CPULimit
	}
	if overrides.MemoryLimit != nil {
		if req.Limits == nil {
			req.Limits = corev1.ResourceList{}
		}
		req.Limits[corev1.ResourceMemory] = *overrides.MemoryLimit
	}

	clampRequestToLimit(&req, corev1.ResourceCPU)
	clampRequestToLimit(&req, corev1.ResourceMemory)

	return req
}

func clampRequestToLimit(req *corev1.ResourceRequirements, name corev1.ResourceName) {
	if req.Limits == nil {
		return
	}
	lim, hasLim := req.Limits[name]
	if !hasLim {
		return
	}
	rq, hasRq := req.Requests[name]
	if !hasRq {
		return
	}
	if rq.Cmp(lim) > 0 {
		klog.Warningf("%s request %s exceeds limit %s; clamping request to limit", name, rq.String(), lim.String())
		req.Requests[name] = lim
	}
}

// String renders a MountResources as a short debug string.
func (r MountResources) String() string {
	return fmt.Sprintf("cpu={req:%s lim:%s} mem={req:%s lim:%s}",
		qstr(r.CPURequest), qstr(r.CPULimit),
		qstr(r.MemoryRequest), qstr(r.MemoryLimit))
}

func qstr(q *resource.Quantity) string {
	if q == nil {
		return "-"
	}
	return q.String()
}
