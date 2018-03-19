package cpumanager

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/api/core/v1"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
)

func getCacheCOS(pod *v1.Pod) string {
	// Skip container of k8s system, keep them in root cache COS
	if pod.Namespace == metav1.NamespaceSystem {
		return ""
	}

	// Assign container to "guaranteed" cache COS if its qos is "guaranteed"
	if v1qos.GetPodQOS(pod) == v1.PodQOSGuaranteed {
		return "guaranteed"
	}

	return "shared"
}
