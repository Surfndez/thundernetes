/*
Copyright 2021.

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

package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mpsv1alpha1 "github.com/playfab/thundernetes/operator/api/v1alpha1"

	hm "github.com/cornelk/hashmap"
)

var (
	ownerKey = ".metadata.controller"
	apiGVStr = mpsv1alpha1.GroupVersion.String()

	podsUnderCreation = &hm.HashMap{}
)

const safeToEvictPodAttribute string = "cluster-autoscaler.kubernetes.io/safe-to-evict"
const finalizerName string = "gameservers.mps.playfab.com/finalizer"

// GameServerReconciler reconciles a GameServer object
type GameServerReconciler struct {
	client.Client
	Scheme                     *runtime.Scheme
	Recorder                   record.EventRecorder
	PortRegistry               *PortRegistry
	GetPublicIpForNodeProvider func(ctx context.Context, r client.Reader, nodeName string) (string, error) // we abstract this for testing purposes
}

// we request secret RBAC access here so they can be potentially used by the API service (for GameServer allocations)

//+kubebuilder:rbac:groups=mps.playfab.com,resources=gameservers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=mps.playfab.com,resources=gameservers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=mps.playfab.com,resources=gameservers/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.8.3/pkg/reconcile
func (r *GameServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var gs mpsv1alpha1.GameServer
	if err := r.Get(ctx, req.NamespacedName, &gs); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Unable to fetch GameServer - skipping")
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch GameServer")
		return ctrl.Result{}, err
	}

	// ----------------------- finalizer logic start ----------------------- //
	// examine DeletionTimestamp to determine if object is under deletion
	if gs.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// registering our finalizer.
		if !containsString(gs.GetFinalizers(), finalizerName) {
			controllerutil.AddFinalizer(&gs, finalizerName)
			if err := r.Update(ctx, &gs); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	} else {
		// The object is being deleted
		if containsString(gs.GetFinalizers(), finalizerName) {
			// our finalizer is present, so lets handle any external dependency
			r.unassignPorts(&gs)
			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(&gs, finalizerName)
			if err := r.Update(ctx, &gs); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}
	// ----------------------- finalizer logic end ----------------------- //

	// get the pod that is owned by this GameServer
	var pod corev1.Pod
	podFoundInCache := true
	if err := r.Get(ctx, types.NamespacedName{Namespace: gs.Namespace, Name: gs.Name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			podFoundInCache = false
		} else {
			// there has been an error other than NotFound
			return ctrl.Result{}, err
		}
	}

	_, podUnderCreation := podsUnderCreation.Get(gs.Name)
	// we have zero pods for this game server and we have recorded that one is being created
	if !podFoundInCache && podUnderCreation {
		// pod is being created, cache hasn't been updated yet
		return ctrl.Result{}, nil
	} else if podUnderCreation {
		podsUnderCreation.Del(gs.Name)
	}

	if !podFoundInCache {
		log.Info("Creating a new pod for GameServer", GameServerKind, gs.Name)
		newPod := NewPodForGameServer(&gs)
		if err := r.Create(ctx, newPod); err != nil {
			return ctrl.Result{}, err
		}
		podsUnderCreation.Set(gs.Name, struct{}{})
		r.Recorder.Eventf(&gs, corev1.EventTypeNormal, "Created", "Created new pod %s for GameServer %s", newPod.Name, gs.Name)
		return ctrl.Result{}, nil
	}

	// check if the pod process has exited (i.e. GameServer session has exited gracefully or crashed)
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Name == SidecarContainerName {
			continue
		}
		if !containerStatus.Ready && containerStatus.State.Terminated != nil {
			exitCode := containerStatus.State.Terminated.ExitCode
			r.Recorder.Eventf(&gs, corev1.EventTypeNormal, "GameServerProcessExited", "GameServer process exited with code %d", exitCode)
			if exitCode == 0 {
				gs.Status.State = mpsv1alpha1.GameServerStateGameCompleted
			} else {
				gs.Status.State = mpsv1alpha1.GameServerStateCrashed
			}
			// updating GameServer with the new state
			if err := r.Status().Update(ctx, &gs); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// other status updates on the GameServer state are provided by the sidecar
	// which calls the K8s API server

	// if a game server is active, there are players present.
	// When using the cluster autoscaler, an annotation will be added
	// to prevent the node from being scaled down.
	r.Recorder.Eventf(&gs, corev1.EventTypeNormal, "Update", "Gameserver %s state is %s", gs.Name, gs.Status.State)
	podAnnotations := pod.GetAnnotations()
	if podAnnotations == nil {
		podAnnotations = make(map[string]string)
	}
	if gs.Status.State == mpsv1alpha1.GameServerStateActive {
		// if the game is active, mark the pod as unsafe to be evicted
		podAnnotations[safeToEvictPodAttribute] = "false"
	} else {
		// game is not active, it is safe to evict this pod
		podAnnotations[safeToEvictPodAttribute] = "true"
	}
	pod.SetAnnotations(podAnnotations)
	r.Update(ctx, &pod)

	// if we don't have a Public IP set, we need to get and set it on the status
	if gs.Status.PublicIP == "" {
		if pod.Spec.NodeName == "" {
			// nodename is empty, maybe the Pod hasn't been scheduled yet?
			return ctrl.Result{}, nil // will requeue when the Pod is scheduled
		}
		publicIP, err := r.GetPublicIpForNodeProvider(ctx, r, pod.Spec.NodeName)
		if err != nil {
			return ctrl.Result{}, err
		}
		gs.Status.PublicIP = publicIP
		gs.Status.Ports = getContainerHostPortTuples(&pod)
		err = r.Status().Update(ctx, &gs)
		if err != nil {
			if apierrors.IsConflict(err) { // there might be a conflict because the sidecar can update the .Status of the GameServer
				return ctrl.Result{Requeue: true}, nil
			} else {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}

// unassignPorts will remove any ports that are used by this GameServer from the port registry
func (r *GameServerReconciler) unassignPorts(gs *mpsv1alpha1.GameServer) {
	hostPorts := make([]int32, 0)
	for i := 0; i < len(gs.Spec.PodSpec.Containers); i++ {
		container := gs.Spec.PodSpec.Containers[i]
		for j := 0; j < len(container.Ports); j++ {
			if sliceContainsPortToExpose(gs.Spec.PortsToExpose, container.Name, container.Ports[j].Name) {
				hostPorts = append(hostPorts, container.Ports[j].HostPort)
			}
		}
	}
	r.PortRegistry.DeregisterServerPorts(hostPorts)
}

// SetupWithManager sets up the controller with the Manager.
func (r *GameServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, ownerKey, func(rawObj client.Object) []string {
		// grab the Pod object, extract the owner...
		pod := rawObj.(*corev1.Pod)
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			return nil
		}
		// ...make sure it's a GameServer...
		if owner.APIVersion != apiGVStr || owner.Kind != "GameServer" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mpsv1alpha1.GameServer{}).
		Owns(&corev1.Pod{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(r)
}
