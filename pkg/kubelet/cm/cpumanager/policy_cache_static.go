package cpumanager

import (
	"k8s.io/api/core/v1"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
)

func getCacheCOS(pod *v1.Pod) string {
	if v1qos.GetPodQOS(pod) == v1.PodQOSGuaranteed {
		return "guaranteed"
	}

	return "shared"
}
