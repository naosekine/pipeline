/*
Copyright 2019 The Tekton Authors

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

package pod

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"

	"knative.dev/pkg/kmeta"

	"github.com/tektoncd/pipeline/pkg/apis/config"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/pod"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"github.com/tektoncd/pipeline/pkg/names"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"knative.dev/pkg/changeset"
)

const (
	// TektonHermeticEnvVar is the env var we set in containers to indicate they should be run hermetically
	TektonHermeticEnvVar = "TEKTON_HERMETIC"

	// ExecutionModeAnnotation is an experimental optional annotation to set the execution mode on a TaskRun
	ExecutionModeAnnotation = "experimental.tekton.dev/execution-mode"

	// ExecutionModeHermetic indicates hermetic execution mode
	ExecutionModeHermetic = "hermetic"

	// deadlineFactor is the factor we multiply the taskrun timeout with to determine the activeDeadlineSeconds of the Pod.
	// It has to be higher than the timeout (to not be killed before)
	deadlineFactor = 1.5
)

// These are effectively const, but Go doesn't have such an annotation.
var (
	ReleaseAnnotation = "pipeline.tekton.dev/release"

	groupVersionKind = schema.GroupVersionKind{
		Group:   v1beta1.SchemeGroupVersion.Group,
		Version: v1beta1.SchemeGroupVersion.Version,
		Kind:    "TaskRun",
	}
	// These are injected into all of the source/step containers.
	implicitVolumeMounts = []corev1.VolumeMount{{
		Name:      "tekton-internal-workspace",
		MountPath: pipeline.WorkspaceDir,
	}, {
		Name:      "tekton-internal-home",
		MountPath: pipeline.HomeDir,
	}, {
		Name:      "tekton-internal-results",
		MountPath: pipeline.DefaultResultPath,
	}, {
		Name:      "tekton-internal-steps",
		MountPath: pipeline.StepsDir,
		ReadOnly:  true,
	}}
	implicitVolumes = []corev1.Volume{{
		Name:         "tekton-internal-workspace",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}, {
		Name:         "tekton-internal-home",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}, {
		Name:         "tekton-internal-results",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}, {
		Name:         "tekton-internal-steps",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}

	// MaxActiveDeadlineSeconds is a maximum permitted value to be used for a task with no timeout
	MaxActiveDeadlineSeconds = int64(math.MaxInt32)
)

// Builder exposes options to configure Pod construction from TaskSpecs/Runs.
type Builder struct {
	Images          pipeline.Images
	KubeClient      kubernetes.Interface
	EntrypointCache EntrypointCache
}

// Transformer is a function that will transform a Pod. This can be used to mutate
// a Pod generated by Tekton after it got generated.
type Transformer func(*corev1.Pod) (*corev1.Pod, error)

// Build creates a Pod using the configuration options set on b and the TaskRun
// and TaskSpec provided in its arguments. An error is returned if there are
// any problems during the conversion.
func (b *Builder) Build(ctx context.Context, taskRun *v1beta1.TaskRun, taskSpec v1beta1.TaskSpec, transformers ...Transformer) (*corev1.Pod, error) {
	var (
		scriptsInit                                       *corev1.Container
		initContainers, stepContainers, sidecarContainers []corev1.Container
		volumes                                           []corev1.Volume
	)
	volumeMounts := []corev1.VolumeMount{binROMount}
	implicitEnvVars := []corev1.EnvVar{}
	alphaAPIEnabled := config.FromContextOrDefaults(ctx).FeatureFlags.EnableAPIFields == config.AlphaAPIFields

	// Add our implicit volumes first, so they can be overridden by the user if they prefer.
	volumes = append(volumes, implicitVolumes...)
	volumeMounts = append(volumeMounts, implicitVolumeMounts...)

	// Create Volumes and VolumeMounts for any credentials found in annotated
	// Secrets, along with any arguments needed by Step entrypoints to process
	// those secrets.
	credEntrypointArgs, credVolumes, credVolumeMounts, err := credsInit(ctx, taskRun.Spec.ServiceAccountName, taskRun.Namespace, b.KubeClient)
	if err != nil {
		return nil, err
	}
	volumes = append(volumes, credVolumes...)
	volumeMounts = append(volumeMounts, credVolumeMounts...)

	// Merge step template with steps.
	// TODO(#1605): Move MergeSteps to pkg/pod
	steps, err := v1beta1.MergeStepsWithStepTemplate(taskSpec.StepTemplate, taskSpec.Steps)
	if err != nil {
		return nil, err
	}

	// Convert any steps with Script to command+args.
	// If any are found, append an init container to initialize scripts.
	if alphaAPIEnabled {
		scriptsInit, stepContainers, sidecarContainers = convertScripts(b.Images.ShellImage, b.Images.ShellImageWin, steps, taskSpec.Sidecars, taskRun.Spec.Debug)
	} else {
		scriptsInit, stepContainers, sidecarContainers = convertScripts(b.Images.ShellImage, "", steps, taskSpec.Sidecars, nil)
	}
	if scriptsInit != nil {
		initContainers = append(initContainers, *scriptsInit)
		volumes = append(volumes, scriptsVolume)
	}

	if alphaAPIEnabled && taskRun.Spec.Debug != nil {
		volumes = append(volumes, debugScriptsVolume, debugInfoVolume)
	}

	// Initialize any workingDirs under /workspace.
	if workingDirInit := workingDirInit(b.Images.WorkingDirInitImage, stepContainers); workingDirInit != nil {
		initContainers = append(initContainers, *workingDirInit)
	}

	// Resolve entrypoint for any steps that don't specify command.
	stepContainers, err = resolveEntrypoints(ctx, b.EntrypointCache, taskRun.Namespace, taskRun.Spec.ServiceAccountName, stepContainers)
	if err != nil {
		return nil, err
	}

	// Rewrite steps with entrypoint binary. Append the entrypoint init
	// container to place the entrypoint binary. Also add timeout flags
	// to entrypoint binary.
	entrypointInit := corev1.Container{
		Name:  "place-tools",
		Image: b.Images.EntrypointImage,
		// Rewrite default WorkingDir from "/home/nonroot" to "/"
		// as suggested at https://github.com/GoogleContainerTools/distroless/issues/718
		// to avoid permission errors with nonroot users not equal to `65532`
		WorkingDir: "/",
		// Invoke the entrypoint binary in "cp mode" to copy itself
		// into the correct location for later steps.
		Command:      []string{"/ko-app/entrypoint", "cp", "/ko-app/entrypoint", entrypointBinary},
		VolumeMounts: []corev1.VolumeMount{binMount},
	}

	if alphaAPIEnabled {
		stepContainers, err = orderContainers(credEntrypointArgs, stepContainers, &taskSpec, taskRun.Spec.Debug)
	} else {
		stepContainers, err = orderContainers(credEntrypointArgs, stepContainers, &taskSpec, nil)
	}
	if err != nil {
		return nil, err
	}
	// place the entrypoint first in case other init containers rely on its
	// features (e.g. decode-script).
	initContainers = append([]corev1.Container{
		entrypointInit,
		tektonDirInit(b.Images.EntrypointImage, steps),
	}, initContainers...)
	volumes = append(volumes, binVolume, downwardVolume)

	// Add implicit env vars.
	// They're prepended to the list, so that if the user specified any
	// themselves their value takes precedence.
	if len(implicitEnvVars) > 0 {
		for i, s := range stepContainers {
			env := append(implicitEnvVars, s.Env...) //nolint
			stepContainers[i].Env = env
		}
	}

	// Add env var if hermetic execution was requested & if the alpha API is enabled
	if taskRun.Annotations[ExecutionModeAnnotation] == ExecutionModeHermetic && alphaAPIEnabled {
		for i, s := range stepContainers {
			// Add it at the end so it overrides
			env := append(s.Env, corev1.EnvVar{Name: TektonHermeticEnvVar, Value: "1"}) //nolint
			stepContainers[i].Env = env
		}
	}

	// Add implicit volume mounts to each step, unless the step specifies
	// its own volume mount at that path.
	for i, s := range stepContainers {
		// Mount /tekton/creds with a fresh volume for each Step. It needs to
		// be world-writeable and empty so creds can be initialized in there. Cant
		// guarantee what UID container runs with. If legacy credential helper (creds-init)
		// is disabled via feature flag then these can be nil since we don't want to mount
		// the automatic credential volume.
		v, vm := getCredsInitVolume(ctx, i)
		if v != nil && vm != nil {
			volumes = append(volumes, *v)
			s.VolumeMounts = append(s.VolumeMounts, *vm)
		}

		// Add /tekton/run state volumes.
		// Each step should only mount their own volume as RW,
		// all other steps should be mounted RO.
		volumes = append(volumes, runVolume(i))
		for j := 0; j < len(stepContainers); j++ {
			s.VolumeMounts = append(s.VolumeMounts, runMount(j, i != j))
		}

		requestedVolumeMounts := map[string]bool{}
		for _, vm := range s.VolumeMounts {
			requestedVolumeMounts[filepath.Clean(vm.MountPath)] = true
		}
		var toAdd []corev1.VolumeMount
		for _, imp := range volumeMounts {
			if !requestedVolumeMounts[filepath.Clean(imp.MountPath)] {
				toAdd = append(toAdd, imp)
			}
		}
		vms := append(s.VolumeMounts, toAdd...) //nolint
		stepContainers[i].VolumeMounts = vms
	}

	// This loop:
	// - sets container name to add "step-" prefix or "step-unnamed-#" if not specified.
	// TODO(#1605): Remove this loop and make each transformation in
	// isolation.
	for i, s := range stepContainers {
		stepContainers[i].Name = names.SimpleNameGenerator.RestrictLength(StepName(s.Name, i))
	}

	// By default, use an empty pod template and take the one defined in the task run spec if any
	podTemplate := pod.Template{}

	if taskRun.Spec.PodTemplate != nil {
		podTemplate = *taskRun.Spec.PodTemplate
	}

	// Add podTemplate Volumes to the explicitly declared use volumes
	volumes = append(volumes, taskSpec.Volumes...)
	volumes = append(volumes, podTemplate.Volumes...)

	if err := v1beta1.ValidateVolumes(volumes); err != nil {
		return nil, err
	}

	mergedPodContainers := stepContainers

	// Merge sidecar containers with step containers.
	for _, sc := range sidecarContainers {
		sc.Name = names.SimpleNameGenerator.RestrictLength(fmt.Sprintf("%v%v", sidecarPrefix, sc.Name))
		mergedPodContainers = append(mergedPodContainers, sc)
	}

	var dnsPolicy corev1.DNSPolicy
	if podTemplate.DNSPolicy != nil {
		dnsPolicy = *podTemplate.DNSPolicy
	}

	var priorityClassName string
	if podTemplate.PriorityClassName != nil {
		priorityClassName = *podTemplate.PriorityClassName
	}

	podAnnotations := kmeta.CopyMap(taskRun.Annotations)
	version, err := changeset.Get()
	if err != nil {
		return nil, err
	}
	podAnnotations[ReleaseAnnotation] = version

	if shouldAddReadyAnnotationOnPodCreate(ctx, taskSpec.Sidecars) {
		podAnnotations[readyAnnotation] = readyAnnotationValue
	}

	// calculate the activeDeadlineSeconds based on the specified timeout (uses default timeout if it's not specified)
	activeDeadlineSeconds := int64(taskRun.GetTimeout(ctx).Seconds() * deadlineFactor)
	// set activeDeadlineSeconds to the max. allowed value i.e. max int32 when timeout is explicitly set to 0
	if taskRun.GetTimeout(ctx) == config.NoTimeoutDuration {
		activeDeadlineSeconds = MaxActiveDeadlineSeconds
	}

	podNameSuffix := "-pod"
	if taskRunRetries := len(taskRun.Status.RetriesStatus); taskRunRetries > 0 {
		podNameSuffix = fmt.Sprintf("%s-retry%d", podNameSuffix, taskRunRetries)
	}
	newPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// We execute the build's pod in the same namespace as where the build was
			// created so that it can access colocated resources.
			Namespace: taskRun.Namespace,
			// Generate a unique name based on the build's name.
			// The name is univocally generated so that in case of
			// stale informer cache, we never create duplicate Pods
			Name: kmeta.ChildName(taskRun.Name, podNameSuffix),
			// If our parent TaskRun is deleted, then we should be as well.
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(taskRun, groupVersionKind),
			},
			Annotations: podAnnotations,
			Labels:      makeLabels(taskRun),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			InitContainers:               initContainers,
			Containers:                   mergedPodContainers,
			ServiceAccountName:           taskRun.Spec.ServiceAccountName,
			Volumes:                      volumes,
			NodeSelector:                 podTemplate.NodeSelector,
			Tolerations:                  podTemplate.Tolerations,
			Affinity:                     podTemplate.Affinity,
			SecurityContext:              podTemplate.SecurityContext,
			RuntimeClassName:             podTemplate.RuntimeClassName,
			AutomountServiceAccountToken: podTemplate.AutomountServiceAccountToken,
			SchedulerName:                podTemplate.SchedulerName,
			HostNetwork:                  podTemplate.HostNetwork,
			DNSPolicy:                    dnsPolicy,
			DNSConfig:                    podTemplate.DNSConfig,
			EnableServiceLinks:           podTemplate.EnableServiceLinks,
			PriorityClassName:            priorityClassName,
			ImagePullSecrets:             podTemplate.ImagePullSecrets,
			HostAliases:                  podTemplate.HostAliases,
			ActiveDeadlineSeconds:        &activeDeadlineSeconds, // Set ActiveDeadlineSeconds to mark the pod as "terminating" (like a Job)
		},
	}

	for _, f := range transformers {
		newPod, err = f(newPod)
		if err != nil {
			return newPod, err
		}
	}

	return newPod, nil
}

// makeLabels constructs the labels we will propagate from TaskRuns to Pods.
func makeLabels(s *v1beta1.TaskRun) map[string]string {
	labels := make(map[string]string, len(s.ObjectMeta.Labels)+1)
	// NB: Set this *before* passing through TaskRun labels. If the TaskRun
	// has a managed-by label, it should override this default.

	// Copy through the TaskRun's labels to the underlying Pod's.
	for k, v := range s.ObjectMeta.Labels {
		labels[k] = v
	}

	// NB: Set this *after* passing through TaskRun Labels. If the TaskRun
	// specifies this label, it should be overridden by this value.
	labels[pipeline.TaskRunLabelKey] = s.Name
	return labels
}

// shouldAddReadyAnnotationonPodCreate returns a bool indicating whether the
// controller should add the `Ready` annotation when creating the Pod. We cannot
// add the annotation if Tekton is running in a cluster with injected sidecars
// or if the Task specifies any sidecars.
func shouldAddReadyAnnotationOnPodCreate(ctx context.Context, sidecars []v1beta1.Sidecar) bool {
	// If the TaskRun has sidecars, we cannot set the READY annotation early
	if len(sidecars) > 0 {
		return false
	}
	// If the TaskRun has no sidecars, check if we are running in a cluster where sidecars can be injected by other
	// controllers.
	cfg := config.FromContextOrDefaults(ctx)
	return !cfg.FeatureFlags.RunningInEnvWithInjectedSidecars
}

func runMount(i int, ro bool) corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      fmt.Sprintf("%s-%d", runVolumeName, i),
		MountPath: filepath.Join(runDir, strconv.Itoa(i)),
		ReadOnly:  ro,
	}
}

func runVolume(i int) corev1.Volume {
	return corev1.Volume{
		Name:         fmt.Sprintf("%s-%d", runVolumeName, i),
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

func tektonDirInit(image string, steps []v1beta1.Step) corev1.Container {
	cmd := make([]string, 0, len(steps)+2)
	cmd = append(cmd, "/ko-app/entrypoint", "step-init")
	for i, s := range steps {
		cmd = append(cmd, StepName(s.Name, i))
	}

	return corev1.Container{
		Name:       "step-init",
		Image:      image,
		WorkingDir: "/",
		Command:    cmd,
		VolumeMounts: []corev1.VolumeMount{{
			Name:      "tekton-internal-steps",
			MountPath: pipeline.StepsDir,
		}},
	}
}
