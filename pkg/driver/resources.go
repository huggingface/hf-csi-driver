package driver

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

// volumeAttributes keys recognised by the driver to configure the FUSE
// daemon container resources (mount pod or injected sidecar).
const (
	volumeCtxCPULimit      = "cpuLimit"
	volumeCtxCPURequest    = "cpuRequest"
	volumeCtxMemoryLimit   = "memoryLimit"
	volumeCtxMemoryRequest = "memoryRequest"
)

// Default resource requests applied to the FUSE daemon container (mount
// pod or injected sidecar) when the user does not set cpuRequest /
// memoryRequest in volumeAttributes.
var (
	DefaultMountCPURequest    = resource.MustParse("10m")
	DefaultMountMemoryRequest = resource.MustParse("32Mi")
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
		CPULimit:      parseQuantityAttr(volumeCtxCPULimit, attrs[volumeCtxCPULimit]),
		CPURequest:    parseQuantityAttr(volumeCtxCPURequest, attrs[volumeCtxCPURequest]),
		MemoryLimit:   parseQuantityAttr(volumeCtxMemoryLimit, attrs[volumeCtxMemoryLimit]),
		MemoryRequest: parseQuantityAttr(volumeCtxMemoryRequest, attrs[volumeCtxMemoryRequest]),
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

// ToStringMap serialises MountResources to a string->string map suitable
// for round-tripping through an unstructured Kubernetes object (e.g. the
// HFMount CRD spec). Empty fields are omitted so the stored map is minimal.
func (r MountResources) ToStringMap() map[string]string {
	m := map[string]string{}
	if r.CPULimit != nil {
		m[volumeCtxCPULimit] = r.CPULimit.String()
	}
	if r.CPURequest != nil {
		m[volumeCtxCPURequest] = r.CPURequest.String()
	}
	if r.MemoryLimit != nil {
		m[volumeCtxMemoryLimit] = r.MemoryLimit.String()
	}
	if r.MemoryRequest != nil {
		m[volumeCtxMemoryRequest] = r.MemoryRequest.String()
	}
	return m
}

// MountResourcesFromUnstructured reads a MountResources out of an unstructured
// spec map (four optional string fields keyed like volumeAttributes). Missing
// or non-string entries yield nil fields.
func MountResourcesFromUnstructured(raw map[string]interface{}) MountResources {
	attrs := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			attrs[k] = s
		}
	}
	return ParseMountResources(attrs)
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
