/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cpumanager

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/resctrl"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/resctrl/bitmap"
	"math"
)

// PolicyStatic is the name of the static policy
const PolicyStatic policyName = "static"

var _ Policy = &staticPolicy{}

// staticPolicy is a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
//
// This policy allocates CPUs exclusively for a container if all the following
// conditions are met:
//
// - The pod QoS class is Guaranteed.
// - The CPU request is a positive integer.
//
// The static policy maintains the following sets of logical CPUs:
//
// - SHARED: Burstable, BestEffort, and non-integral Guaranteed containers
//   run here. Initially this contains all CPU IDs on the system. As
//   exclusive allocations are created and destroyed, this CPU set shrinks
//   and grows, accordingly. This is stored in the state as the default
//   CPU set.
//
// - RESERVED: A subset of the shared pool which is not exclusively
//   allocatable. The membership of this pool is static for the lifetime of
//   the Kubelet. The size of the reserved pool is
//   ceil(systemreserved.cpu + kubereserved.cpu).
//   Reserved CPUs are taken topologically starting with lowest-indexed
//   physical core, as reported by cAdvisor.
//
// - ASSIGNABLE: Equal to SHARED - RESERVED. Exclusive CPUs are allocated
//   from this pool.
//
// - EXCLUSIVE ALLOCATIONS: CPU sets assigned exclusively to one container.
//   These are stored as explicit assignments in the state.
//
// When an exclusive allocation is made, the static policy also updates the
// default cpuset in the state abstraction. The CPU manager's periodic
// reconcile loop takes care of rewriting the cpuset in cgroupfs for any
// containers that may be running in the shared pool. For this reason,
// applications running within exclusively-allocated containers must tolerate
// potentially sharing their allocated CPUs for up to the CPU manager
// reconcile period.
type staticPolicy struct {
	// cpu socket topology
	topology *topology.CPUTopology
	// set of CPUs that is not available for exclusive assignment
	reserved cpuset.CPUSet
	// percentage of LLC to be shared with containers that have burstable or best-effort SLA
	llcSharedPercentage int32
	// percentage of LLC to be used for benchmarking
	llcBenchPercentage int32
}

// Ensure staticPolicy implements Policy interface
var _ Policy = &staticPolicy{}

// NewStaticPolicy returns a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
func NewStaticPolicy(topology *topology.CPUTopology, numReservedCPUs int, llcSharedPercentage int32, llcBenchPercentage int32) Policy {
	allCPUs := topology.CPUDetails.CPUs()
	// takeByTopology allocates CPUs associated with low-numbered cores from
	// allCPUs.
	//
	// For example: Given a system with 8 CPUs available and HT enabled,
	// if numReservedCPUs=2, then reserved={0,4}
	reserved, _ := takeByTopology(topology, allCPUs, numReservedCPUs)

	if reserved.Size() != numReservedCPUs {
		panic(fmt.Sprintf("[cpumanager] unable to reserve the required amount of CPUs (size of %s did not equal %d)", reserved, numReservedCPUs))
	}

	glog.Infof("[cpumanager] reserved %d CPUs (\"%s\") not available for exclusive assignment", reserved.Size(), reserved)

	return &staticPolicy{
		topology:            topology,
		reserved:            reserved,
		llcSharedPercentage: llcSharedPercentage,
		llcBenchPercentage:  llcBenchPercentage,
	}
}

func (p *staticPolicy) Name() string {
	return string(PolicyStatic)
}

func (p *staticPolicy) Start(s state.State) {
	if err := p.validateState(s); err != nil {
		glog.Errorf("[cpumanager] static policy invalid state: %s\n", err.Error())
		panic("[cpumanager] - please drain node and remove policy state file")
	}

	if !resctrl.IsIntelRdtMounted() {
		glog.Errorf("[cpumanager] resctrl not mounted\n")
		panic("[cpumanager] - resctrl not mounted")
	}

	if err := p.validateLLCParams(); err != nil {
		glog.Errorf("[cpumanager] invalid LLC params: %s\n", err.Error())
		panic("[cpumanager] - invalid LLC params")
	}

	if err := p.setupLLC(); err != nil {
		glog.Errorf("[cpumanager] unable to setup resctrl: %s\n", err.Error())
		panic("[cpumanager] - unable to setup resctrl")
	}
}

func (p *staticPolicy) validateLLCParams() error {
	if p.llcSharedPercentage < 1 || p.llcSharedPercentage > 100 {
		return fmt.Errorf("LLC shared Cache CoS should occupy atleast 1 percentage; User provided value=%d", p.llcSharedPercentage)
	}
	return nil
}
func (p *staticPolicy) setupLLC() error {

	glog.V(1).Infof("[cpumanager] creating %v percent guaranteed and %v percent shared Cache CoS", p.llcBenchPercentage, p.llcSharedPercentage)

	commitLLC("guaranteed", p.llcBenchPercentage)

	commitLLC("shared", p.llcSharedPercentage)

	return nil
}

func commitLLC(groupName string, llcPercentage int32) error {

	// Get LLC level
	level := resctrl.GetLLC()
	cacheLevel := "L" + strconv.FormatUint(uint64(level), 10)
	rdtCoSInfo := resctrl.GetRdtCosInfo()
	if rdtCoSInfo[strings.ToLower(cacheLevel)] == nil {
		return fmt.Errorf("unable to fetch CAT info")
	}

	// Determine number of cache ways
	cbmMask := resctrl.GetRdtCosInfo()[strings.ToLower(cacheLevel)].CbmMask
	maskLen := bitmap.CbmLen(cbmMask)
	sharedWays := math.Floor(float64(llcPercentage) / float64(100) * float64(maskLen))
	glog.V(1).Infof("[cpumanager] shared ways %f for shared %v percentage in %s group", sharedWays, llcPercentage, groupName)

	b, err := bitmap.NewBitmap(maskLen, cbmMask)
	if err != nil {
		return fmt.Errorf("CAT bitmask generation failed: %s for %s group", err.Error(), groupName)
	}

	r := b.GetConnectiveBits(uint32(sharedWays), 0, true)
	glog.V(1).Infof("[cpumanager] shared COS bitmask %s for shared %v percentage in %s group", r.ToString(), llcPercentage, groupName)

	// Determine number of available LLC
	num, err := resctrl.GetNumberOfCaches(int(level))
	if err != nil {
		return fmt.Errorf("unable to fetch CPU cache info: %s", err.Error())
	}

	res := resctrl.NewResAssociation()

	res.Schemata[cacheLevel] = make([]resctrl.CacheCos, num)
	for i := 0; i < num; i++ {
		res.Schemata[cacheLevel][i] = resctrl.CacheCos{ID: uint8(i), Mask: r.ToString()}
	}

	err = resctrl.Commit(res, groupName)
	if err != nil {
		return fmt.Errorf("commit failed for %s Cache CoS: %s", groupName, err.Error())
	}


	return nil
}
func (p *staticPolicy) validateState(s state.State) error {
	tmpAssignments := s.GetCPUAssignments()
	tmpDefaultCPUset := s.GetDefaultCPUSet()

	// Default cpuset cannot be empty when assignments exist
	if tmpDefaultCPUset.IsEmpty() {
		if len(tmpAssignments) != 0 {
			return fmt.Errorf("default cpuset cannot be empty")
		}
		// state is empty initialize
		allCPUs := p.topology.CPUDetails.CPUs()
		s.SetDefaultCPUSet(allCPUs)
		return nil
	}

	// State has already been initialized from file (is not empty)
	// 1 Check if the reserved cpuset is not part of default cpuset because:
	// - kube/system reserved have changed (increased) - may lead to some containers not being able to start
	// - user tampered with file
	if !p.reserved.Intersection(tmpDefaultCPUset).Equals(p.reserved) {
		return fmt.Errorf("not all reserved cpus: \"%s\" are present in defaultCpuSet: \"%s\"",
			p.reserved.String(), tmpDefaultCPUset.String())
	}

	// 2. Check if state for static policy is consistent
	for cID, cset := range tmpAssignments {
		// None of the cpu in DEFAULT cset should be in s.assignments
		if !tmpDefaultCPUset.Intersection(cset).IsEmpty() {
			return fmt.Errorf("container id: %s cpuset: \"%s\" overlaps with default cpuset \"%s\"",
				cID, cset.String(), tmpDefaultCPUset.String())
		}
	}
	return nil
}

// assignableCPUs returns the set of unassigned CPUs minus the reserved set.
func (p *staticPolicy) assignableCPUs(s state.State) cpuset.CPUSet {
	return s.GetDefaultCPUSet().Difference(p.reserved)
}

func (p *staticPolicy) AddContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {
	if numCPUs := guaranteedCPUs(pod, container); numCPUs != 0 {
		glog.Infof("[cpumanager] static policy: AddContainer (pod: %s, container: %s, container id: %s)", pod.Name, container.Name, containerID)
		// container belongs in an exclusively allocated pool

		if _, ok := s.GetCPUSet(containerID); ok {
			glog.Infof("[cpumanager] static policy: container already present in state, skipping (container: %s, container id: %s)", container.Name, containerID)
			return nil
		}

		cpuset, err := p.allocateCPUs(s, numCPUs)
		if err != nil {
			glog.Errorf("[cpumanager] unable to allocate %d CPUs (container id: %s, error: %v)", numCPUs, containerID, err)
			return err
		}
		s.SetCPUSet(containerID, cpuset)
	}
	// container belongs in the shared pool (nothing to do; use default cpuset)
	return nil
}

func (p *staticPolicy) UpdateContainer(s state.State, pod *v1.Pod, container *v1.Container, containerID string) error {
	glog.Infof("[cpumanager] static policy: UpdateContainer (pod: %s, container: %s, container id: %s)", pod.Name, container.Name, containerID)

	if _, ok := s.GetLLCSchema(containerID); ok {
		glog.Infof("[cpumanager] static policy: container already present in state, skipping (container: %s, container id: %s)", container.Name, containerID)
		return nil
	}
	var (
		cmdOut []byte
		err    error
	)
	cmdName := "docker"
	cmdArgs := []string{"inspect", "--format={{.State.Pid}}", containerID}
	if cmdOut, err = exec.Command(cmdName, cmdArgs...).Output(); err != nil {
		glog.Errorf("There was an error running docker inspect command: %v", err)
		return err
	}
	processID := string(cmdOut)

	cacheCOS := getCacheCOS(pod)
	// Skip container of k8s system, keep them in root cache COS
	if cacheCOS == "" {
		glog.V(1).Infof("[cpumanager] Skip update cache for container %s in namespace %s", containerID, pod.Namespace)
		return nil
	}
	// After get processID, do cache allocation action here for this process
	res := resctrl.NewResAssociation()
	res.Tasks = append(res.Tasks, processID)
	glog.V(1).Infof("[cpumanager] Update container %s process %s to cache %s COS", containerID, processID, cacheCOS)
	resctrl.Commit(res, cacheCOS)

	s.SetLLCSchema(containerID, cacheCOS)

	return nil
}

func (p *staticPolicy) RemoveContainer(s state.State, containerID string) error {
	glog.Infof("[cpumanager] static policy: RemoveContainer (container id: %s)", containerID)
	if toRelease, ok := s.GetCPUSet(containerID); ok {
		s.Delete(containerID)
		// Mutate the shared pool, adding released cpus.
		s.SetDefaultCPUSet(s.GetDefaultCPUSet().Union(toRelease))
	}
	return nil
}

func (p *staticPolicy) allocateCPUs(s state.State, numCPUs int) (cpuset.CPUSet, error) {
	glog.Infof("[cpumanager] allocateCpus: (numCPUs: %d)", numCPUs)
	result, err := takeByTopology(p.topology, p.assignableCPUs(s), numCPUs)
	if err != nil {
		return cpuset.NewCPUSet(), err
	}
	// Remove allocated CPUs from the shared CPUSet.
	s.SetDefaultCPUSet(s.GetDefaultCPUSet().Difference(result))

	glog.Infof("[cpumanager] allocateCPUs: returning \"%v\"", result)
	return result, nil
}

func guaranteedCPUs(pod *v1.Pod, container *v1.Container) int {
	if v1qos.GetPodQOS(pod) != v1.PodQOSGuaranteed {
		return 0
	}
	cpuQuantity := container.Resources.Requests[v1.ResourceCPU]
	if cpuQuantity.Value()*1000 != cpuQuantity.MilliValue() {
		return 0
	}
	// Safe downcast to do for all systems with < 2.1 billion CPUs.
	// Per the language spec, `int` is guaranteed to be at least 32 bits wide.
	// https://golang.org/ref/spec#Numeric_types
	return int(cpuQuantity.Value())
}
