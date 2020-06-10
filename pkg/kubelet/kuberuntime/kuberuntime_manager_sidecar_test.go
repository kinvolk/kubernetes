/*
Copyright 2016 The Kubernetes Authors.

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

package kuberuntime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/flowcontrol"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

func TestSyncPodWithSidecarsAndInitContainers(t *testing.T) {
	fakeRuntime, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	initContainers := []v1.Container{
		{
			Name:            "init1",
			Image:           "init",
			ImagePullPolicy: v1.PullIfNotPresent,
		},
	}
	containers := []v1.Container{
		{
			Name:            "foo1",
			Image:           "busybox",
			ImagePullPolicy: v1.PullIfNotPresent,
		},
		{
			Name:            "foo2",
			Image:           "alpine",
			ImagePullPolicy: v1.PullIfNotPresent,
		},
	}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "foo",
			Namespace: "new",
			Annotations: map[string]string{
				"alpha.kinvolk.io/sidecar": `["foo2"]`,
			},
		},
		Spec: v1.PodSpec{
			Containers:     containers,
			InitContainers: initContainers,
		},
	}

	backOff := flowcontrol.NewBackOff(time.Second, time.Minute)

	// 1. should only create the init container.
	podStatus, err := m.GetPodStatus(pod.UID, pod.Name, pod.Namespace)
	assert.NoError(t, err)
	result := m.SyncPod(pod, podStatus, []v1.Secret{}, backOff)
	assert.NoError(t, result.Error())
	expected := []*cRecord{
		{name: initContainers[0].Name, attempt: 0, state: runtimeapi.ContainerState_CONTAINER_RUNNING},
	}
	verifyContainerStatuses(t, fakeRuntime, expected, "start only the init container")

	// 2. should not create app container because init container is still running.
	podStatus, err = m.GetPodStatus(pod.UID, pod.Name, pod.Namespace)
	assert.NoError(t, err)
	result = m.SyncPod(pod, podStatus, []v1.Secret{}, backOff)
	assert.NoError(t, result.Error())
	verifyContainerStatuses(t, fakeRuntime, expected, "init container still running; do nothing")

	// 3. should create all sidecar containers because init container finished.
	// Stop init container instance 0.
	sandboxIDs, err := m.getSandboxIDByPodUID(pod.UID, nil)
	require.NoError(t, err)
	sandboxID := sandboxIDs[0]
	initID0, err := fakeRuntime.GetContainerID(sandboxID, initContainers[0].Name, 0)
	require.NoError(t, err)
	fakeRuntime.StopContainer(initID0, 0)
	// Sync again.
	pod.Status.ContainerStatuses = []v1.ContainerStatus{
		{
			Name:  "foo1",
			State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}},
		},
		{
			Name:  "foo2",
			State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}},
		},
	}
	podStatus, err = m.GetPodStatus(pod.UID, pod.Name, pod.Namespace)
	assert.NoError(t, err)
	result = m.SyncPod(pod, podStatus, []v1.Secret{}, backOff)
	assert.NoError(t, result.Error())
	expected = []*cRecord{
		{name: initContainers[0].Name, attempt: 0, state: runtimeapi.ContainerState_CONTAINER_EXITED},
		{name: containers[1].Name, attempt: 0, state: runtimeapi.ContainerState_CONTAINER_RUNNING},
	}
	verifyContainerStatuses(t, fakeRuntime, expected, "init container completed; all sidecar containers should be running")

	//4 Should start non-sidecars once sidecars are ready
	pod.Status.ContainerStatuses = []v1.ContainerStatus{
		{
			Name:  "foo1",
			State: v1.ContainerState{Running: &v1.ContainerStateRunning{}},
		},
		{
			Name:  "foo2",
			Ready: true,
			State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{}},
		},
	}
	podStatus, err = m.GetPodStatus(pod.UID, pod.Name, pod.Namespace)
	assert.NoError(t, err)
	result = m.SyncPod(pod, podStatus, []v1.Secret{}, backOff)
	assert.NoError(t, result.Error())
	expected = []*cRecord{
		{name: initContainers[0].Name, attempt: 0, state: runtimeapi.ContainerState_CONTAINER_EXITED},
		{name: containers[0].Name, attempt: 0, state: runtimeapi.ContainerState_CONTAINER_RUNNING},
		{name: containers[1].Name, attempt: 0, state: runtimeapi.ContainerState_CONTAINER_RUNNING},
	}
	verifyContainerStatuses(t, fakeRuntime, expected, "init container completed, sidecars ready; all containers should be running")

	// 5. should restart the init container if needed to create a new podsandbox
	// Stop the pod sandbox.
	fakeRuntime.StopPodSandbox(sandboxID)
	// Sync again.
	podStatus, err = m.GetPodStatus(pod.UID, pod.Name, pod.Namespace)
	assert.NoError(t, err)
	result = m.SyncPod(pod, podStatus, []v1.Secret{}, backOff)
	assert.NoError(t, result.Error())
	expected = []*cRecord{
		// The first init container instance is purged and no longer visible.
		// The second (attempt == 1) instance has been started and is running.
		{name: initContainers[0].Name, attempt: 1, state: runtimeapi.ContainerState_CONTAINER_RUNNING},
		// All containers are killed.
		{name: containers[0].Name, attempt: 0, state: runtimeapi.ContainerState_CONTAINER_EXITED},
		{name: containers[1].Name, attempt: 0, state: runtimeapi.ContainerState_CONTAINER_EXITED},
	}
	verifyContainerStatuses(t, fakeRuntime, expected, "kill all app containers, purge the existing init container, and restart a new one")
}

func TestComputePodActionsWithSidecarsAndInitContainers(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	require.NoError(t, err)

	// Createing a pair reference pod and status for the test cases to refer
	// the specific fields.
	basePod, baseStatus := makeBasePodAndStatusWithSidecarsAndInitContainers()
	noAction := podActions{
		SandboxID:         baseStatus.SandboxStatuses[0].Id,
		ContainersToStart: []int{},
		ContainersToKill:  map[kubecontainer.ContainerID]containerToKillInfo{},
	}

	for desc, test := range map[string]struct {
		mutatePodFn    func(*v1.Pod)
		mutateStatusFn func(*kubecontainer.PodStatus)
		actions        podActions
	}{
		"initialization completed; start all sidecar containers": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Status.ContainerStatuses = []v1.ContainerStatus{
					{
						Name: "foo1",
						State: v1.ContainerState{
							Waiting: &v1.ContainerStateWaiting{},
						},
					},
					{
						Name: "foo2",
						State: v1.ContainerState{
							Waiting: &v1.ContainerStateWaiting{},
						},
					},
					{
						Name: "foo3",
						State: v1.ContainerState{
							Waiting: &v1.ContainerStateWaiting{},
						},
					},
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{1},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
		"initialization in progress; do nothing": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyAlways },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[2].State = kubecontainer.ContainerStateRunning
			},
			actions: noAction,
		},
		"Kill pod and restart the first init container if the pod sandbox is dead": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyAlways },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.SandboxStatuses[0].State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
			},
			actions: podActions{
				KillPod:                  true,
				CreateSandbox:            true,
				SandboxID:                baseStatus.SandboxStatuses[0].Id,
				Attempt:                  uint32(1),
				NextInitContainerToStart: &basePod.Spec.InitContainers[0],
				ContainersToStart:        []int{},
				ContainersToKill:         getKillMapWithInitContainers(basePod, baseStatus, []int{}),
			},
		},
		"initialization failed; restart the last init container if RestartPolicy == Always": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyAlways },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[2].ExitCode = 137
			},
			actions: podActions{
				SandboxID:                baseStatus.SandboxStatuses[0].Id,
				NextInitContainerToStart: &basePod.Spec.InitContainers[2],
				ContainersToStart:        []int{},
				ContainersToKill:         getKillMapWithInitContainers(basePod, baseStatus, []int{}),
			},
		},
		"initialization failed; restart the last init container if RestartPolicy == OnFailure": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyOnFailure },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[2].ExitCode = 137
			},
			actions: podActions{
				SandboxID:                baseStatus.SandboxStatuses[0].Id,
				NextInitContainerToStart: &basePod.Spec.InitContainers[2],
				ContainersToStart:        []int{},
				ContainersToKill:         getKillMapWithInitContainers(basePod, baseStatus, []int{}),
			},
		},
		"initialization failed; kill pod if RestartPolicy == Never": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyNever },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[2].ExitCode = 137
			},
			actions: podActions{
				KillPod:           true,
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{},
				ContainersToKill:  getKillMapWithInitContainers(basePod, baseStatus, []int{}),
			},
		},
		"init container state unknown; kill and recreate the last init container if RestartPolicy == Always": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyAlways },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[2].State = kubecontainer.ContainerStateUnknown
			},
			actions: podActions{
				SandboxID:                baseStatus.SandboxStatuses[0].Id,
				NextInitContainerToStart: &basePod.Spec.InitContainers[2],
				ContainersToStart:        []int{},
				ContainersToKill:         getKillMapWithInitContainers(basePod, baseStatus, []int{2}),
			},
		},
		"init container state unknown; kill and recreate the last init container if RestartPolicy == OnFailure": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyOnFailure },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[2].State = kubecontainer.ContainerStateUnknown
			},
			actions: podActions{
				SandboxID:                baseStatus.SandboxStatuses[0].Id,
				NextInitContainerToStart: &basePod.Spec.InitContainers[2],
				ContainersToStart:        []int{},
				ContainersToKill:         getKillMapWithInitContainers(basePod, baseStatus, []int{2}),
			},
		},
		"init container state unknown; kill pod if RestartPolicy == Never": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyNever },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[2].State = kubecontainer.ContainerStateUnknown
			},
			actions: podActions{
				KillPod:           true,
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{},
				ContainersToKill:  getKillMapWithInitContainers(basePod, baseStatus, []int{}),
			},
		},
		"Pod sandbox not ready, init container failed, but RestartPolicy == Never; kill pod only": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyNever },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.SandboxStatuses[0].State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
			},
			actions: podActions{
				KillPod:           true,
				CreateSandbox:     false,
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				Attempt:           uint32(1),
				ContainersToStart: []int{},
				ContainersToKill:  getKillMapWithInitContainers(basePod, baseStatus, []int{}),
			},
		},
		"Pod sandbox not ready, and RestartPolicy == Never, but no visible init containers;  create a new pod sandbox": {
			mutatePodFn: func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyNever },
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.SandboxStatuses[0].State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
				status.ContainerStatuses = []*kubecontainer.ContainerStatus{}
			},
			actions: podActions{
				KillPod:                  true,
				CreateSandbox:            true,
				SandboxID:                baseStatus.SandboxStatuses[0].Id,
				Attempt:                  uint32(1),
				NextInitContainerToStart: &basePod.Spec.InitContainers[0],
				ContainersToStart:        []int{},
				ContainersToKill:         getKillMapWithInitContainers(basePod, baseStatus, []int{}),
			},
		},
	} {
		pod, status := makeBasePodAndStatusWithSidecarsAndInitContainers()
		if test.mutatePodFn != nil {
			test.mutatePodFn(pod)
		}
		if test.mutateStatusFn != nil {
			test.mutateStatusFn(status)
		}
		actions := m.computePodActions(pod, status)
		verifyActions(t, &test.actions, &actions, desc)
	}
}

func makeBasePodAndStatusWithSidecarsAndInitContainers() (*v1.Pod, *kubecontainer.PodStatus) {
	pod, status := makeBasePodAndStatusWithInitContainers()
	pod.Annotations = map[string]string{"alpha.kinvolk.io/sidecar": `["foo2"]`}
	return pod, status
}

func makeBasePodAndStatusWithSidecar() (*v1.Pod, *kubecontainer.PodStatus) {
	pod, status := makeBasePodAndStatus()
	pod.Annotations = map[string]string{"alpha.kinvolk.io/sidecar": `["foo2"]`}
	status.ContainerStatuses[1].Hash = kubecontainer.HashContainer(&pod.Spec.Containers[1])

	return pod, status
}

func TestComputePodActionsWithSidecar(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	require.NoError(t, err)

	// Creating a pair reference pod and status for the test cases to refer
	// the specific fields.
	basePod, baseStatus := makeBasePodAndStatusWithSidecar()
	for desc, test := range map[string]struct {
		mutatePodFn    func(*v1.Pod)
		mutateStatusFn func(*kubecontainer.PodStatus)
		actions        podActions
	}{
		"Kill sidecars if all non-sidecars are terminated": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyNever
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						continue
					}
					status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
					status.ContainerStatuses[i].ExitCode = 0
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{1}),
				ContainersToStart: []int{},
			},
		},
		"Kill remaining sidecars if non-sidecars and sidecar are terminated": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyNever
				pod.ObjectMeta.Annotations = map[string]string{
					"alpha.kinvolk.io/sidecar": `["foo2", "foo3"]`,
				}
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						continue
					}
					status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
					status.ContainerStatuses[i].ExitCode = 0
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{1}),
				ContainersToStart: []int{},
			},
		},
		"Restart container if all non-sidecars are terminated, restart policy always": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyAlways
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
					status.ContainerStatuses[i].ExitCode = 0
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
				ContainersToStart: []int{0, 1, 2},
			},
		},
		"Restart sidecar container while non-sidecars are running, restart policy onfailure": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyOnFailure
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[1].State = kubecontainer.ContainerStateExited
				status.ContainerStatuses[1].ExitCode = 1
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
				ContainersToStart: []int{1},
			},
		},
		"Don't restart succeeded sidecar container while non-sidecars are running, restart policy onfailure": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyOnFailure
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[1].State = kubecontainer.ContainerStateExited
				status.ContainerStatuses[1].ExitCode = 0
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
				ContainersToStart: []int{},
			},
		},
		"Don't restart succeeded sidecar container while non-sidecars are running, restart policy never": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyNever
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[1].State = kubecontainer.ContainerStateExited
				status.ContainerStatuses[1].ExitCode = 0
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
				ContainersToStart: []int{},
			},
		},
		"Restart sidecar in unknown state": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyNever
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[1].State = kubecontainer.ContainerStateUnknown
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{1}),
				ContainersToStart: []int{1},
			},
		},
		"Don't restart unknown sidecar if all non-sidecars have exited, restart policy never": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyNever
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						status.ContainerStatuses[i].State = kubecontainer.ContainerStateUnknown
					}
					status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
					status.ContainerStatuses[i].ExitCode = 1
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				KillPod:           true,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
				ContainersToStart: []int{},
			},
		},
		"Don't restart unknown sidecar if all non-sidecars have exited, restart policy on failure": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyOnFailure
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						status.ContainerStatuses[i].State = kubecontainer.ContainerStateUnknown
					}
					status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
					status.ContainerStatuses[i].ExitCode = 0
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				KillPod:           true,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
				ContainersToStart: []int{},
			},
		},
		"Everything is running, do nothing": {
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
				ContainersToStart: []int{},
			},
		},
		"Kill pod if sidecar terminates with failure but all non-sidecars succeeded with restart policy on failure": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyOnFailure
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
						status.ContainerStatuses[i].ExitCode = 1
					}
					status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
					status.ContainerStatuses[i].ExitCode = 0
				}
			},
			actions: podActions{
				KillPod:           true,
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
				ContainersToStart: []int{},
			},
		},
		"Kill pod if all containers have terminated": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyNever
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
					status.ContainerStatuses[i].ExitCode = 0
				}
			},
			actions: podActions{
				KillPod:           true,
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
				ContainersToStart: []int{},
			},
		},
		"Start sidecar containers before non-sidecars when creating a new pod": {
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				// No container or sandbox exists.
				status.SandboxStatuses = []*runtimeapi.PodSandboxStatus{}
				status.ContainerStatuses = []*kubecontainer.ContainerStatus{}
			},
			actions: podActions{
				KillPod:           true,
				CreateSandbox:     true,
				Attempt:           uint32(0),
				ContainersToStart: []int{1},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
		"Don't start non-sidecars until sidecars are ready": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Status.ContainerStatuses = []v1.ContainerStatus{
					{
						Name: "foo1",
					},
					{
						Name:  "foo2",
						Ready: false,
					},
					{
						Name: "foo3",
					},
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
		"Start non-sidecars when sidecars are ready": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Status.ContainerStatuses = []v1.ContainerStatus{
					{
						Name: "foo1",
						State: v1.ContainerState{
							Waiting: &v1.ContainerStateWaiting{},
						},
					},
					{
						Name:  "foo2",
						Ready: true,
					},
					{
						Name: "foo3",
						State: v1.ContainerState{
							Waiting: &v1.ContainerStateWaiting{},
						},
					},
				}
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						continue
					}
					status.ContainerStatuses[i].State = ""
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{0, 2},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
		"Start non-sidecar with no state": {
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[2].State = ""
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{2},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
		"Start non-sidecar with no state and sidecar failed": {
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				status.ContainerStatuses[2].State = ""
				status.ContainerStatuses[1].State = kubecontainer.ContainerStateExited
				status.ContainerStatuses[1].ExitCode = 1
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{1, 2},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
		"Start non-sidecars if pod status is nil": {
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						continue
					}
					status.ContainerStatuses[i].State = ""
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{0, 2},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
		"Restart only sidecars while non-sidecars are waiting": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyAlways
				pod.Status.ContainerStatuses = []v1.ContainerStatus{
					{
						Name: "foo1",
					},
					{
						Name:  "foo2",
						Ready: false,
					},
					{
						Name: "foo3",
					},
				}
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
					}
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{1},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
		"Restart only sidecars while non-sidecars are waiting, restart policy on failure": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyOnFailure
				pod.Status.ContainerStatuses = []v1.ContainerStatus{
					{
						Name: "foo1",
						State: v1.ContainerState{
							Waiting: &v1.ContainerStateWaiting{},
						},
					},
					{
						Name:  "foo2",
						Ready: false,
					},
					{
						Name: "foo3",
						State: v1.ContainerState{
							Waiting: &v1.ContainerStateWaiting{},
						},
					},
				}
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
						status.ContainerStatuses[i].ExitCode = 1
					}
					status.ContainerStatuses[i].State = ""
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{1},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
		"Restart running non-sidecars despite sidecar becoming not ready": {
			mutatePodFn: func(pod *v1.Pod) {
				pod.Spec.RestartPolicy = v1.RestartPolicyAlways
				pod.Status.ContainerStatuses = []v1.ContainerStatus{
					{
						Name: "foo1",
					},
					{
						Name:  "foo2",
						Ready: false,
					},
					{
						Name: "foo3",
					},
				}
			},
			mutateStatusFn: func(status *kubecontainer.PodStatus) {
				for i := range status.ContainerStatuses {
					if i == 1 {
						continue
					}
					status.ContainerStatuses[i].State = kubecontainer.ContainerStateExited
				}
			},
			actions: podActions{
				SandboxID:         baseStatus.SandboxStatuses[0].Id,
				ContainersToStart: []int{0, 2},
				ContainersToKill:  getKillMap(basePod, baseStatus, []int{}),
			},
		},
	} {
		pod, status := makeBasePodAndStatusWithSidecar()
		if test.mutatePodFn != nil {
			test.mutatePodFn(pod)
		}
		if test.mutateStatusFn != nil {
			test.mutateStatusFn(status)
		}
		actions := m.computePodActions(pod, status)
		verifyActions(t, &test.actions, &actions, desc)
	}
}
