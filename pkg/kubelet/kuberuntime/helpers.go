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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type UsernsAnn int

const (
	UsernsInvalid UsernsAnn = iota
	UsernsRuntimeDefault
	UsernsPod
	UsernsNode
)

type podsByID []*kubecontainer.Pod

func (b podsByID) Len() int           { return len(b) }
func (b podsByID) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b podsByID) Less(i, j int) bool { return b[i].ID < b[j].ID }

type containersByID []*kubecontainer.Container

func (b containersByID) Len() int           { return len(b) }
func (b containersByID) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b containersByID) Less(i, j int) bool { return b[i].ID.ID < b[j].ID.ID }

// Newest first.
type podSandboxByCreated []*runtimeapi.PodSandbox

func (p podSandboxByCreated) Len() int           { return len(p) }
func (p podSandboxByCreated) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p podSandboxByCreated) Less(i, j int) bool { return p[i].CreatedAt > p[j].CreatedAt }

type containerStatusByCreated []*kubecontainer.ContainerStatus

func (c containerStatusByCreated) Len() int           { return len(c) }
func (c containerStatusByCreated) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }
func (c containerStatusByCreated) Less(i, j int) bool { return c[i].CreatedAt.After(c[j].CreatedAt) }

// toKubeContainerState converts runtimeapi.ContainerState to kubecontainer.ContainerState.
func toKubeContainerState(state runtimeapi.ContainerState) kubecontainer.ContainerState {
	switch state {
	case runtimeapi.ContainerState_CONTAINER_CREATED:
		return kubecontainer.ContainerStateCreated
	case runtimeapi.ContainerState_CONTAINER_RUNNING:
		return kubecontainer.ContainerStateRunning
	case runtimeapi.ContainerState_CONTAINER_EXITED:
		return kubecontainer.ContainerStateExited
	case runtimeapi.ContainerState_CONTAINER_UNKNOWN:
		return kubecontainer.ContainerStateUnknown
	}

	return kubecontainer.ContainerStateUnknown
}

// toRuntimeProtocol converts v1.Protocol to runtimeapi.Protocol.
func toRuntimeProtocol(protocol v1.Protocol) runtimeapi.Protocol {
	switch protocol {
	case v1.ProtocolTCP:
		return runtimeapi.Protocol_TCP
	case v1.ProtocolUDP:
		return runtimeapi.Protocol_UDP
	case v1.ProtocolSCTP:
		return runtimeapi.Protocol_SCTP
	}

	klog.Warningf("Unknown protocol %q: defaulting to TCP", protocol)
	return runtimeapi.Protocol_TCP
}

// toKubeContainer converts runtimeapi.Container to kubecontainer.Container.
func (m *kubeGenericRuntimeManager) toKubeContainer(c *runtimeapi.Container) (*kubecontainer.Container, error) {
	if c == nil || c.Id == "" || c.Image == nil {
		return nil, fmt.Errorf("unable to convert a nil pointer to a runtime container")
	}

	annotatedInfo := getContainerInfoFromAnnotations(c.Annotations)
	return &kubecontainer.Container{
		ID:      kubecontainer.ContainerID{Type: m.runtimeName, ID: c.Id},
		Name:    c.GetMetadata().GetName(),
		ImageID: c.ImageRef,
		Image:   c.Image.Image,
		Hash:    annotatedInfo.Hash,
		State:   toKubeContainerState(c.State),
	}, nil
}

// sandboxToKubeContainer converts runtimeapi.PodSandbox to kubecontainer.Container.
// This is only needed because we need to return sandboxes as if they were
// kubecontainer.Containers to avoid substantial changes to PLEG.
// TODO: Remove this once it becomes obsolete.
func (m *kubeGenericRuntimeManager) sandboxToKubeContainer(s *runtimeapi.PodSandbox) (*kubecontainer.Container, error) {
	if s == nil || s.Id == "" {
		return nil, fmt.Errorf("unable to convert a nil pointer to a runtime container")
	}

	return &kubecontainer.Container{
		ID:    kubecontainer.ContainerID{Type: m.runtimeName, ID: s.Id},
		State: kubecontainer.SandboxToContainerState(s.State),
	}, nil
}

// getImageUser gets uid or user name that will run the command(s) from image. The function
// guarantees that only one of them is set.
func (m *kubeGenericRuntimeManager) getImageUser(image string) (*int64, string, error) {
	imageStatus, err := m.imageService.ImageStatus(&runtimeapi.ImageSpec{Image: image})
	if err != nil {
		return nil, "", err
	}

	if imageStatus != nil {
		if imageStatus.Uid != nil {
			return &imageStatus.GetUid().Value, "", nil
		}

		if imageStatus.Username != "" {
			return nil, imageStatus.Username, nil
		}
	}

	// If non of them is set, treat it as root.
	return new(int64), "", nil
}

// isInitContainerFailed returns true if container has exited and exitcode is not zero
// or is in unknown state.
func isInitContainerFailed(status *kubecontainer.ContainerStatus) bool {
	if status.State == kubecontainer.ContainerStateExited && status.ExitCode != 0 {
		return true
	}

	if status.State == kubecontainer.ContainerStateUnknown {
		return true
	}

	return false
}

// getStableKey generates a key (string) to uniquely identify a
// (pod, container) tuple. The key should include the content of the
// container, so that any change to the container generates a new key.
func getStableKey(pod *v1.Pod, container *v1.Container) string {
	hash := strconv.FormatUint(kubecontainer.HashContainer(container), 16)
	return fmt.Sprintf("%s_%s_%s_%s_%s", pod.Name, pod.Namespace, string(pod.UID), container.Name, hash)
}

// logPathDelimiter is the delimiter used in the log path.
const logPathDelimiter = "_"

// buildContainerLogsPath builds log path for container relative to pod logs directory.
func buildContainerLogsPath(containerName string, restartCount int) string {
	return filepath.Join(containerName, fmt.Sprintf("%d.log", restartCount))
}

// BuildContainerLogsDirectory builds absolute log directory path for a container in pod.
func BuildContainerLogsDirectory(podNamespace, podName string, podUID types.UID, containerName string) string {
	return filepath.Join(BuildPodLogsDirectory(podNamespace, podName, podUID), containerName)
}

// BuildPodLogsDirectory builds absolute log directory path for a pod sandbox.
func BuildPodLogsDirectory(podNamespace, podName string, podUID types.UID) string {
	return filepath.Join(podLogsRootDirectory, strings.Join([]string{podNamespace, podName,
		string(podUID)}, logPathDelimiter))
}

// parsePodUIDFromLogsDirectory parses pod logs directory name and returns the pod UID.
// It supports both the old pod log directory /var/log/pods/UID, and the new pod log
// directory /var/log/pods/NAMESPACE_NAME_UID.
func parsePodUIDFromLogsDirectory(name string) types.UID {
	parts := strings.Split(name, logPathDelimiter)
	return types.UID(parts[len(parts)-1])
}

// toKubeRuntimeStatus converts the runtimeapi.RuntimeStatus to kubecontainer.RuntimeStatus.
func toKubeRuntimeStatus(status *runtimeapi.RuntimeStatus) *kubecontainer.RuntimeStatus {
	conditions := []kubecontainer.RuntimeCondition{}
	for _, c := range status.GetConditions() {
		conditions = append(conditions, kubecontainer.RuntimeCondition{
			Type:    kubecontainer.RuntimeConditionType(c.Type),
			Status:  c.Status,
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	return &kubecontainer.RuntimeStatus{Conditions: conditions}
}

// toKubeRuntimeConfig converts the runtimeapi.ActiveRuntimeConfig to kubecontainer.RuntimeConfigInfo
func toKubeRuntimeConfig(config *runtimeapi.ActiveRuntimeConfig) *kubecontainer.RuntimeConfigInfo {
	usernsConfig := config.GetUserNamespaceConfig()
	if usernsConfig == nil {
		return &kubecontainer.RuntimeConfigInfo{}
	}
	uidMappingsRuntime := usernsConfig.GetUidMappings()
	if uidMappingsRuntime == nil || len(uidMappingsRuntime) == 0 {
		return &kubecontainer.RuntimeConfigInfo{}
	}
	gidMappingsRuntime := usernsConfig.GetGidMappings()
	if gidMappingsRuntime == nil || len(gidMappingsRuntime) == 0 {
		return &kubecontainer.RuntimeConfigInfo{}
	}
	var uidMappings []*kubecontainer.UserNSMapping
	var gidMappings []*kubecontainer.UserNSMapping
	for _, runtimeMapping := range uidMappingsRuntime {
		uidMappings = append(uidMappings, &kubecontainer.UserNSMapping{
			ContainerID: runtimeMapping.ContainerId,
			HostID:      runtimeMapping.HostId,
			Size:        runtimeMapping.Size_})
	}
	for _, runtimeMapping := range gidMappingsRuntime {
		gidMappings = append(gidMappings, &kubecontainer.UserNSMapping{
			ContainerID: runtimeMapping.ContainerId,
			HostID:      runtimeMapping.HostId,
			Size:        runtimeMapping.Size_})
	}
	userNSConfig := kubecontainer.UserNamespaceConfigInfo{
		UidMappings: uidMappings,
		GidMappings: gidMappings,
	}
	return &kubecontainer.RuntimeConfigInfo{UserNamespaceConfig: userNSConfig}
}

// getSeccompProfileFromAnnotations gets seccomp profile from annotations.
// It gets pod's profile if containerName is empty.
func (m *kubeGenericRuntimeManager) getSeccompProfileFromAnnotations(annotations map[string]string, containerName string) string {
	// try the pod profile.
	profile, profileOK := annotations[v1.SeccompPodAnnotationKey]
	if containerName != "" {
		// try the container profile.
		cProfile, cProfileOK := annotations[v1.SeccompContainerAnnotationKeyPrefix+containerName]
		if cProfileOK {
			profile = cProfile
			profileOK = cProfileOK
		}
	}

	if !profileOK {
		return ""
	}

	if strings.HasPrefix(profile, "localhost/") {
		name := strings.TrimPrefix(profile, "localhost/")
		fname := filepath.Join(m.seccompProfileRoot, filepath.FromSlash(name))
		return "localhost/" + fname
	}

	return profile
}

func ipcNamespaceForPod(pod *v1.Pod) runtimeapi.NamespaceMode {
	if pod != nil && pod.Spec.HostIPC {
		return runtimeapi.NamespaceMode_NODE
	}
	return runtimeapi.NamespaceMode_POD
}

func networkNamespaceForPod(pod *v1.Pod) runtimeapi.NamespaceMode {
	if pod != nil && pod.Spec.HostNetwork {
		return runtimeapi.NamespaceMode_NODE
	}
	return runtimeapi.NamespaceMode_POD
}

func pidNamespaceForPod(pod *v1.Pod) runtimeapi.NamespaceMode {
	if pod != nil {
		if pod.Spec.HostPID {
			return runtimeapi.NamespaceMode_NODE
		}
		if pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace {
			return runtimeapi.NamespaceMode_POD
		}
	}
	// Note that PID does not default to the zero value for v1.Pod
	return runtimeapi.NamespaceMode_CONTAINER
}

// namespacesForPod returns the runtimeapi.NamespaceOption for a given pod.
// An empty or nil pod can be used to get the namespace defaults for v1.Pod.
func (m *kubeGenericRuntimeManager) namespacesForPod(pod *v1.Pod) (*runtimeapi.NamespaceOption, error) {
	user, err := m.userNamespaceForPod(pod)
	if err != nil {
		return nil, err
	}

	return &runtimeapi.NamespaceOption{
		Ipc:     ipcNamespaceForPod(pod),
		Network: networkNamespaceForPod(pod),
		Pid:     pidNamespaceForPod(pod),
		User:    user,
	}, nil
}

func (m *kubeGenericRuntimeManager) userNamespaceForPod(pod *v1.Pod) (runtimeapi.NamespaceMode, error) {
	config, err := m.GetRuntimeConfigInfo()
	if err != nil {
		return runtimeapi.NamespaceMode_NODE, fmt.Errorf("user namespace can't be enabled: %v", err)
	}

	runtimeEnabled := config != nil && config.IsUserNamespaceSupported() && config.IsUserNamespaceEnabled()
	userns := getUserNsAnnotation(pod)

	switch userns {
	case UsernsRuntimeDefault:
		if runtimeEnabled && !m.enableHostUserNamespace(pod) {
			return runtimeapi.NamespaceMode_POD, nil
		}
		return runtimeapi.NamespaceMode_NODE, nil
	case UsernsPod:
		if m.enableHostUserNamespace(pod) || !runtimeEnabled {
			return runtimeapi.NamespaceMode_NODE, fmt.Errorf("user namespace can't be enabled")
		}
		return runtimeapi.NamespaceMode_POD, nil
	case UsernsNode:
		return runtimeapi.NamespaceMode_NODE, nil
	default:
		return runtimeapi.NamespaceMode_NODE, fmt.Errorf("invalid value for user ns annotation")
	}
}

// enableHostUserNamespace determines if the host user namespace should be used by the container runtime.
// Returns true if the pod is using a host pid, pic, or network namespace, the pod is using a non-namespaced
// capability, the pod contains a privileged container, or the pod has a host path volume.
//
// NOTE: when if a container shares any namespace with another container it must also share the user namespace
// or it will not have the correct capabilities in the namespace.  This means that host user namespace
// is enabled per pod, not per container.
func (m *kubeGenericRuntimeManager) enableHostUserNamespace(pod *v1.Pod) bool {
	if pod == nil {
		return false
	}
	if kubecontainer.HasPrivilegedContainer(pod) || hasHostNamespace(pod) ||
		hasHostVolume(pod) || hasNonNamespacedCapability(pod) || m.hasHostMountPVC(pod) {
		return true
	}
	return false
}

// hasNonNamespacedCapability returns true if MKNOD, SYS_TIME, or SYS_MODULE is requested for any container.
func hasNonNamespacedCapability(pod *v1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.SecurityContext != nil && c.SecurityContext.Capabilities != nil {
			for _, cap := range c.SecurityContext.Capabilities.Add {
				if cap == "MKNOD" || cap == "SYS_TIME" || cap == "SYS_MODULE" {
					return true
				}
			}
		}
	}

	return false
}

// hasHostVolume returns true if the pod spec has a HostPath volume.
func hasHostVolume(pod *v1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			return true
		}
	}
	return false
}

// hasHostNamespace returns true if hostIPC, hostNetwork, or hostPID are set to true.
func hasHostNamespace(pod *v1.Pod) bool {
	return pod.Spec.HostIPC || pod.Spec.HostNetwork || pod.Spec.HostPID
}

// hasHostMountPVC returns true if a PVC is referencing a HostPath volume.
func (m *kubeGenericRuntimeManager) hasHostMountPVC(pod *v1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			pvc, err := m.kubeClient.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(volume.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("unable to retrieve pvc %s:%s - %v", pod.Namespace, volume.PersistentVolumeClaim.ClaimName, err)
				continue
			}
			if pvc != nil {
				referencedVolume, err := m.kubeClient.CoreV1().PersistentVolumes().Get(pvc.Spec.VolumeName, metav1.GetOptions{})
				if err != nil {
					klog.Warningf("unable to retrieve pv %s - %v", pvc.Spec.VolumeName, err)
					continue
				}
				if referencedVolume != nil && referencedVolume.Spec.HostPath != nil {
					return true
				}
			}
		}
	}
	return false
}

// getUserNsAnnotation returns the value of the user namespace annotation
func getUserNsAnnotation(pod *v1.Pod) UsernsAnn {
	if pod == nil {
		return UsernsRuntimeDefault
	}
	userns, ok := pod.Annotations[kivolkUsernsAnn]
	if !ok {
		return UsernsRuntimeDefault
	}

	switch userns {
	case "pod":
		return UsernsPod
	case "node":
		return UsernsNode
	case "runtimeDefault":
		return UsernsRuntimeDefault
	default:
		return UsernsInvalid
	}
}

// chownAllFilesAt traverses the directory tree at the give path and chowns the paths to adjust for the remapped usernamespaces
func (m *kubeGenericRuntimeManager) chownAllFilesAt(dir string) error {
	var files []string

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		files = append(files, path)
		return nil
	})
	if err != nil {
		return err
	}
	klog.V(5).Infof("Chowned paths %v", files)
	for _, file := range files {
		containerUID := uint32(0)
		containerGID := uint32(0)
		uid, err := m.GetHostUID(containerUID)
		if err != nil {
			return fmt.Errorf("Failed to get remapped host UID corresponding to UID 0 in container namespace: %v", err)
		}
		gid, err := m.GetHostGID(containerGID)
		if err != nil {
			return fmt.Errorf("Failed to get remapped host GID corresponding to GID 0 in container namespace: %v", err)
		}
		klog.V(5).Infof("Remapped default uid %d, default gid %d path %s", uid, gid, file)
		err = os.Lchown(file, int(uid), int(gid))
		if err != nil {
			return err
		}
	}
	return nil
}
