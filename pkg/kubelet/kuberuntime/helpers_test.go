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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	runtimetesting "k8s.io/cri-api/pkg/apis/testing"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

func TestStableKey(t *testing.T) {
	container := &v1.Container{
		Name:  "test_container",
		Image: "foo/image:v1",
	}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test_pod",
			Namespace: "test_pod_namespace",
			UID:       "test_pod_uid",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{*container},
		},
	}
	oldKey := getStableKey(pod, container)

	// Updating the container image should change the key.
	container.Image = "foo/image:v2"
	newKey := getStableKey(pod, container)
	assert.NotEqual(t, oldKey, newKey)
}

func TestToKubeContainer(t *testing.T) {
	c := &runtimeapi.Container{
		Id: "test-id",
		Metadata: &runtimeapi.ContainerMetadata{
			Name:    "test-name",
			Attempt: 1,
		},
		Image:    &runtimeapi.ImageSpec{Image: "test-image"},
		ImageRef: "test-image-ref",
		State:    runtimeapi.ContainerState_CONTAINER_RUNNING,
		Annotations: map[string]string{
			containerHashLabel: "1234",
		},
	}
	expect := &kubecontainer.Container{
		ID: kubecontainer.ContainerID{
			Type: runtimetesting.FakeRuntimeName,
			ID:   "test-id",
		},
		Name:    "test-name",
		ImageID: "test-image-ref",
		Image:   "test-image",
		Hash:    uint64(0x1234),
		State:   kubecontainer.ContainerStateRunning,
	}

	_, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)
	got, err := m.toKubeContainer(c)
	assert.NoError(t, err)
	assert.Equal(t, expect, got)
}

func TestGetImageUser(t *testing.T) {
	_, i, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	type image struct {
		name     string
		uid      *runtimeapi.Int64Value
		username string
	}

	type imageUserValues struct {
		// getImageUser can return (*int64)(nil) so comparing with *uid will break
		// type cannot be *int64 as Golang does not allow to take the address of a numeric constant"
		uid      interface{}
		username string
		err      error
	}

	tests := []struct {
		description             string
		originalImage           image
		expectedImageUserValues imageUserValues
	}{
		{
			"image without username and uid should return (new(int64), \"\", nil)",
			image{
				name:     "test-image-ref1",
				uid:      (*runtimeapi.Int64Value)(nil),
				username: "",
			},
			imageUserValues{
				uid:      int64(0),
				username: "",
				err:      nil,
			},
		},
		{
			"image with username and no uid should return ((*int64)nil, imageStatus.Username, nil)",
			image{
				name:     "test-image-ref2",
				uid:      (*runtimeapi.Int64Value)(nil),
				username: "testUser",
			},
			imageUserValues{
				uid:      (*int64)(nil),
				username: "testUser",
				err:      nil,
			},
		},
		{
			"image with uid should return (*int64, \"\", nil)",
			image{
				name: "test-image-ref3",
				uid: &runtimeapi.Int64Value{
					Value: 2,
				},
				username: "whatever",
			},
			imageUserValues{
				uid:      int64(2),
				username: "",
				err:      nil,
			},
		},
	}

	i.SetFakeImages([]string{"test-image-ref1", "test-image-ref2", "test-image-ref3"})
	for j, test := range tests {
		i.Images[test.originalImage.name].Username = test.originalImage.username
		i.Images[test.originalImage.name].Uid = test.originalImage.uid

		uid, username, err := m.getImageUser(test.originalImage.name)
		assert.NoError(t, err, "TestCase[%d]", j)

		if test.expectedImageUserValues.uid == (*int64)(nil) {
			assert.Equal(t, test.expectedImageUserValues.uid, uid, "TestCase[%d]", j)
		} else {
			assert.Equal(t, test.expectedImageUserValues.uid, *uid, "TestCase[%d]", j)
		}
		assert.Equal(t, test.expectedImageUserValues.username, username, "TestCase[%d]", j)
	}
}

func TestGetSeccompProfileFromAnnotations(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	require.NoError(t, err)

	tests := []struct {
		description     string
		annotation      map[string]string
		containerName   string
		expectedProfile string
	}{
		{
			description:     "no seccomp should return empty string",
			expectedProfile: "",
		},
		{
			description:     "no seccomp with containerName should return exmpty string",
			containerName:   "container1",
			expectedProfile: "",
		},
		{
			description: "pod runtime/default seccomp profile should return runtime/default",
			annotation: map[string]string{
				v1.SeccompPodAnnotationKey: v1.SeccompProfileRuntimeDefault,
			},
			expectedProfile: v1.SeccompProfileRuntimeDefault,
		},
		{
			description: "pod docker/default seccomp profile should return docker/default",
			annotation: map[string]string{
				v1.SeccompPodAnnotationKey: v1.DeprecatedSeccompProfileDockerDefault,
			},
			expectedProfile: v1.DeprecatedSeccompProfileDockerDefault,
		},
		{
			description: "pod runtime/default seccomp profile with containerName should return runtime/default",
			annotation: map[string]string{
				v1.SeccompPodAnnotationKey: v1.SeccompProfileRuntimeDefault,
			},
			containerName:   "container1",
			expectedProfile: v1.SeccompProfileRuntimeDefault,
		},
		{
			description: "pod docker/default seccomp profile with containerName should return docker/default",
			annotation: map[string]string{
				v1.SeccompPodAnnotationKey: v1.DeprecatedSeccompProfileDockerDefault,
			},
			containerName:   "container1",
			expectedProfile: v1.DeprecatedSeccompProfileDockerDefault,
		},
		{
			description: "pod unconfined seccomp profile should return unconfined",
			annotation: map[string]string{
				v1.SeccompPodAnnotationKey: "unconfined",
			},
			expectedProfile: "unconfined",
		},
		{
			description: "pod unconfined seccomp profile with containerName should return unconfined",
			annotation: map[string]string{
				v1.SeccompPodAnnotationKey: "unconfined",
			},
			containerName:   "container1",
			expectedProfile: "unconfined",
		},
		{
			description: "pod localhost seccomp profile should return local profile path",
			annotation: map[string]string{
				v1.SeccompPodAnnotationKey: "localhost/chmod.json",
			},
			expectedProfile: "localhost/" + filepath.Join(fakeSeccompProfileRoot, "chmod.json"),
		},
		{
			description: "pod localhost seccomp profile with containerName should return local profile path",
			annotation: map[string]string{
				v1.SeccompPodAnnotationKey: "localhost/chmod.json",
			},
			containerName:   "container1",
			expectedProfile: "localhost/" + filepath.Join(fakeSeccompProfileRoot, "chmod.json"),
		},
		{
			description: "container localhost seccomp profile with containerName should return local profile path",
			annotation: map[string]string{
				v1.SeccompContainerAnnotationKeyPrefix + "container1": "localhost/chmod.json",
			},
			containerName:   "container1",
			expectedProfile: "localhost/" + filepath.Join(fakeSeccompProfileRoot, "chmod.json"),
		},
		{
			description: "container localhost seccomp profile should override pod profile",
			annotation: map[string]string{
				v1.SeccompPodAnnotationKey:                            "unconfined",
				v1.SeccompContainerAnnotationKeyPrefix + "container1": "localhost/chmod.json",
			},
			containerName:   "container1",
			expectedProfile: "localhost/" + filepath.Join(fakeSeccompProfileRoot, "chmod.json"),
		},
		{
			description: "container localhost seccomp profile with unmatched containerName should return empty string",
			annotation: map[string]string{
				v1.SeccompContainerAnnotationKeyPrefix + "container1": "localhost/chmod.json",
			},
			containerName:   "container2",
			expectedProfile: "",
		},
	}

	for i, test := range tests {
		seccompProfile := m.getSeccompProfileFromAnnotations(test.annotation, test.containerName)
		assert.Equal(t, test.expectedProfile, seccompProfile, "TestCase[%d]", i)
	}
}

func TestNamespacesForPod(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	// Runtime doesn't support user namespaces
	m.runtimeConfigCached = true
	m.runtimeConfig = nil

	for desc, test := range map[string]struct {
		input    *v1.Pod
		expected *runtimeapi.NamespaceOption
	}{
		"nil pod -> default v1 namespaces": {
			nil,
			&runtimeapi.NamespaceOption{
				Ipc:     runtimeapi.NamespaceMode_POD,
				Network: runtimeapi.NamespaceMode_POD,
				Pid:     runtimeapi.NamespaceMode_CONTAINER,
				User:    runtimeapi.NamespaceMode_NODE,
			},
		},
		"v1.Pod default namespaces": {
			&v1.Pod{},
			&runtimeapi.NamespaceOption{
				Ipc:     runtimeapi.NamespaceMode_POD,
				Network: runtimeapi.NamespaceMode_POD,
				Pid:     runtimeapi.NamespaceMode_CONTAINER,
				User:    runtimeapi.NamespaceMode_NODE,
			},
		},
		"Host Namespaces": {
			&v1.Pod{
				Spec: v1.PodSpec{
					HostIPC:     true,
					HostNetwork: true,
					HostPID:     true,
				},
			},
			&runtimeapi.NamespaceOption{
				Ipc:     runtimeapi.NamespaceMode_NODE,
				Network: runtimeapi.NamespaceMode_NODE,
				Pid:     runtimeapi.NamespaceMode_NODE,
				User:    runtimeapi.NamespaceMode_NODE,
			},
		},
		"Shared Process Namespace (feature enabled)": {
			&v1.Pod{
				Spec: v1.PodSpec{
					ShareProcessNamespace: &[]bool{true}[0],
				},
			},
			&runtimeapi.NamespaceOption{
				Ipc:     runtimeapi.NamespaceMode_POD,
				Network: runtimeapi.NamespaceMode_POD,
				Pid:     runtimeapi.NamespaceMode_POD,
				User:    runtimeapi.NamespaceMode_NODE,
			},
		},
		"Shared Process Namespace, redundant flag (feature enabled)": {
			&v1.Pod{
				Spec: v1.PodSpec{
					ShareProcessNamespace: &[]bool{false}[0],
				},
			},
			&runtimeapi.NamespaceOption{
				Ipc:     runtimeapi.NamespaceMode_POD,
				Network: runtimeapi.NamespaceMode_POD,
				Pid:     runtimeapi.NamespaceMode_CONTAINER,
				User:    runtimeapi.NamespaceMode_NODE,
			},
		},
	} {
		t.Logf("TestCase: %s", desc)
		actual, err := m.namespacesForPod(test.input)
		assert.NoError(t, err)
		assert.Equal(t, test.expected, actual)
	}
}

func TestUserNamespaceForPod(t *testing.T) {
	_, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	// Runtime supports user namespaces
	uidMappings := []*kubecontainer.UserNSMapping{
		&kubecontainer.UserNSMapping{
			ContainerID: 0,
			HostID: 1000,
			Size: 65536,
		},
	}
	userNSConfig := kubecontainer.UserNamespaceConfigInfo{
		UidMappings: uidMappings,
		GidMappings: uidMappings,
	}
	m.runtimeConfigCached = true
	// TODO: how to avoid creating a real class and mocking it instead?
	m.runtimeConfig = &kubecontainer.RuntimeConfigInfo{UserNamespaceConfig: userNSConfig}

	//trueObj := true

	for desc, test := range map[string]struct {
		input         *v1.Pod
		expected      runtimeapi.NamespaceMode
		expectedError bool
	}{
		"nil pod -> default v1 namespaces": {
			nil,
			runtimeapi.NamespaceMode_POD,
			false,
		},
		"v1.Pod default namespaces": {
			&v1.Pod{},
			runtimeapi.NamespaceMode_POD,
			false,
		},
		"User ns node mode": {
			&v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						kivolkUsernsAnn: "node",
					},
				},
			},
			runtimeapi.NamespaceMode_NODE,
			false,
		},
		"Host Namespaces": {
			&v1.Pod{
				Spec: v1.PodSpec{
					HostIPC:     true,
					HostNetwork: true,
					HostPID:     true,
				},
			},
			runtimeapi.NamespaceMode_NODE,
			false,
		},
		"Host Namespaces and user ns pod mode": {
			&v1.Pod{
				Spec: v1.PodSpec{
					HostIPC:     true,
					HostNetwork: true,
					HostPID:     true,
				},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						kivolkUsernsAnn: "pod",
					},
				},
			},
			runtimeapi.NamespaceMode_NODE,
			true,
		},
		"Privileged container": {
			&v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						v1.Container{
							SecurityContext: &v1.SecurityContext {
								Privileged: &[]bool{true}[0],
							},
						},
					},
				},
			},
			runtimeapi.NamespaceMode_NODE,
			false,
		},
		"Privileged container user ns pod mode": {
			&v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						v1.Container{
							SecurityContext: &v1.SecurityContext {
								Privileged: &[]bool{true}[0],
							},
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						kivolkUsernsAnn: "pod",
					},
				},
			},
			runtimeapi.NamespaceMode_NODE,
			true,
		},
		"Non namespaced capability": {
			&v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						v1.Container{
							SecurityContext: &v1.SecurityContext {
								Capabilities: &v1.Capabilities {
									Add: []v1.Capability {
										"MKNOD", "SYS_TIME", "SYS_MODULE",
									},
								},
							},
						},
					},
				},
			},
			runtimeapi.NamespaceMode_NODE,
			false,
		},
		"Non namespaced capability with userns pod": {
			&v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						v1.Container{
							SecurityContext: &v1.SecurityContext {
								Capabilities: &v1.Capabilities {
									Add: []v1.Capability {
										"MKNOD", "SYS_TIME", "SYS_MODULE",
									},
								},
							},
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						kivolkUsernsAnn: "pod",
					},
				},
			},
			runtimeapi.NamespaceMode_NODE,
			true,
		},
		"Host path volume": {
			&v1.Pod{
				Spec: v1.PodSpec{
					Volumes: []v1.Volume {
						v1.Volume {
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: "/tmp/anything",
								},
							},
						},
					},
				},
			},
			runtimeapi.NamespaceMode_NODE,
			false,
		},
		"Host path volume with user ns pod": {
			&v1.Pod{
				Spec: v1.PodSpec{
					Volumes: []v1.Volume {
						v1.Volume {
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: "/tmp/anything",
								},
							},
						},
					},
				},
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						kivolkUsernsAnn: "pod",
					},
				},
			},
			runtimeapi.NamespaceMode_NODE,
			true,
		},
		"Bad userns annotation": {
			&v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						kivolkUsernsAnn: "itsbad",
					},
				},
			},
			runtimeapi.NamespaceMode_NODE,
			true,
		},
	} {
		t.Logf("TestCase: %s", desc)
		actual, err := m.userNamespaceForPod(test.input)
		if test.expectedError {
			assert.Error(t, err)
		} else {
			assert.NoError(t, err)
			assert.Equal(t, test.expected, actual)
		}
	}
}

func TestToKubeRuntimeStatus(t *testing.T) {

}
