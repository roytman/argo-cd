package controller

import (
	"context"
	"fmt"
	"sort"
	"sync"

	log "github.com/sirupsen/logrus"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	appv1 "github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/reposerver/repository"
	"github.com/argoproj/argo-cd/util/argo"
	"github.com/argoproj/argo-cd/util/kube"
)

type syncContext struct {
	appName       string
	proj          *appv1.AppProject
	comparison    *appv1.ComparisonResult
	resources     []appv1.ResourceState
	config        *rest.Config
	dynamicIf     dynamic.Interface
	disco         discovery.DiscoveryInterface
	kubectl       kube.Kubectl
	namespace     string
	syncOp        *appv1.SyncOperation
	syncRes       *appv1.SyncOperationResult
	syncResources []appv1.SyncOperationResource
	opState       *appv1.OperationState
	manifestInfo  *repository.ManifestResponse
	log           *log.Entry
	// lock to protect concurrent updates of the result list
	lock sync.Mutex
}

func (s *appStateManager) SyncAppState(app *appv1.Application, state *appv1.OperationState) {
	// Sync requests might be requested with ambiguous revisions (e.g. master, HEAD, v1.2.3).
	// This can change meaning when resuming operations (e.g a hook sync). After calculating a
	// concrete git commit SHA, the SHA is remembered in the status.operationState.syncResult and
	// rollbackResult fields. This ensures that when resuming an operation, we sync to the same
	// revision that we initially started with.
	var revision string
	var syncOp appv1.SyncOperation
	var syncRes *appv1.SyncOperationResult
	var syncResources []appv1.SyncOperationResource
	var overrides []appv1.ComponentParameter

	if state.Operation.Sync != nil {
		syncOp = *state.Operation.Sync
		syncResources = syncOp.Resources
		overrides = []appv1.ComponentParameter(state.Operation.Sync.ParameterOverrides)
		if state.SyncResult != nil {
			syncRes = state.SyncResult
			revision = state.SyncResult.Revision
		} else {
			syncRes = &appv1.SyncOperationResult{}
			state.SyncResult = syncRes
		}
	} else {
		state.Phase = appv1.OperationFailed
		state.Message = "Invalid operation request: no operation specified"
		return
	}

	if revision == "" {
		// if we get here, it means we did not remember a commit SHA which we should be syncing to.
		// This typically indicates we are just about to begin a brand new sync/rollback operation.
		// Take the value in the requested operation. We will resolve this to a SHA later.
		revision = syncOp.Revision
	}

	comparison, manifestInfo, resources, conditions, err := s.CompareAppState(app, revision, overrides)
	if err != nil {
		state.Phase = appv1.OperationError
		state.Message = err.Error()
		return
	}
	errConditions := make([]appv1.ApplicationCondition, 0)
	for i := range conditions {
		if conditions[i].IsError() {
			errConditions = append(errConditions, conditions[i])
		}
	}
	if len(errConditions) > 0 {
		state.Phase = appv1.OperationError
		state.Message = argo.FormatAppConditions(errConditions)
		return
	}
	// We now have a concrete commit SHA. Set this in the sync result revision so that we remember
	// what we should be syncing to when resuming operations.
	syncRes.Revision = manifestInfo.Revision

	clst, err := s.db.GetCluster(context.Background(), app.Spec.Destination.Server)
	if err != nil {
		state.Phase = appv1.OperationError
		state.Message = err.Error()
		return
	}

	restConfig := clst.RESTConfig()
	dynamicIf, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		state.Phase = appv1.OperationError
		state.Message = fmt.Sprintf("Failed to initialize dynamic client: %v", err)
		return
	}
	disco, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		state.Phase = appv1.OperationError
		state.Message = fmt.Sprintf("Failed to initialize discovery client: %v", err)
		return
	}

	proj, err := argo.GetAppProject(&app.Spec, s.appclientset, s.namespace)
	if err != nil {
		state.Phase = appv1.OperationError
		state.Message = fmt.Sprintf("Failed to load application project: %v", err)
		return
	}

	syncCtx := syncContext{
		appName:       app.Name,
		proj:          proj,
		comparison:    comparison,
		config:        restConfig,
		dynamicIf:     dynamicIf,
		disco:         disco,
		kubectl:       s.kubectl,
		namespace:     app.Spec.Destination.Namespace,
		syncOp:        &syncOp,
		syncRes:       syncRes,
		syncResources: syncResources,
		opState:       state,
		manifestInfo:  manifestInfo,
		log:           log.WithFields(log.Fields{"application": app.Name}),
		resources:     resources,
	}

	if state.Phase == appv1.OperationTerminating {
		syncCtx.terminate()
	} else {
		syncCtx.sync()
	}

	if !syncOp.DryRun && len(syncOp.Resources) == 0 && syncCtx.opState.Phase.Successful() {
		err := s.persistDeploymentInfo(app, manifestInfo.Revision, manifestInfo.Params, nil)
		if err != nil {
			state.Phase = appv1.OperationError
			state.Message = fmt.Sprintf("failed to record sync to history: %v", err)
		}
	}
}

// syncTask holds the live and target object. At least one should be non-nil. A targetObj of nil
// indicates the live object needs to be pruned. A liveObj of nil indicates the object has yet to
// be deployed
type syncTask struct {
	liveObj   *unstructured.Unstructured
	targetObj *unstructured.Unstructured
}

// sync has performs the actual apply or hook based sync
func (sc *syncContext) sync() {
	syncTasks, successful := sc.generateSyncTasks()
	if !successful {
		return
	}

	// If no sync tasks were generated (e.g., in case all application manifests have been removed),
	// set the sync operation as successful.
	if len(syncTasks) == 0 {
		sc.setOperationPhase(appv1.OperationSucceeded, "successfully synced (no manifests)")
		return
	}

	// Perform a `kubectl apply --dry-run` against all the manifests. This will detect most (but
	// not all) validation issues with the user's manifests (e.g. will detect syntax issues, but
	// will not not detect if they are mutating immutable fields). If anything fails, we will refuse
	// to perform the sync.
	if !sc.startedPreSyncPhase() {
		// Optimization: we only wish to do this once per operation, performing additional dry-runs
		// is harmless, but redundant. The indicator we use to detect if we have already performed
		// the dry-run for this operation, is if the resource or hook list is empty.
		if !sc.doApplySync(syncTasks, true, false, sc.syncOp.DryRun) {
			sc.setOperationPhase(appv1.OperationFailed, "one or more objects failed to apply (dry run)")
			return
		}
		if sc.syncOp.DryRun {
			sc.setOperationPhase(appv1.OperationSucceeded, "successfully synced (dry run)")
			return
		}
	}

	// All objects passed a `kubectl apply --dry-run`, so we are now ready to actually perform the sync.
	if sc.syncOp.SyncStrategy == nil {
		// default sync strategy to hook if no strategy
		sc.syncOp.SyncStrategy = &appv1.SyncStrategy{Hook: &appv1.SyncStrategyHook{}}
	}
	if sc.syncOp.SyncStrategy.Apply != nil {
		if !sc.startedSyncPhase() {
			if !sc.doApplySync(syncTasks, false, sc.syncOp.SyncStrategy.Apply.Force, true) {
				sc.setOperationPhase(appv1.OperationFailed, "one or more objects failed to apply")
				return
			}
			// If apply was successful, return here and force an app refresh. This is so the app
			// will become requeued into the workqueue, to force a new sync/health assessment before
			// marking the operation as completed
			return
		}
		sc.setOperationPhase(appv1.OperationSucceeded, "successfully synced")
	} else if sc.syncOp.SyncStrategy.Hook != nil {
		hooks, err := sc.getHooks()
		if err != nil {
			sc.setOperationPhase(appv1.OperationError, fmt.Sprintf("failed to generate hooks resources: %v", err))
			return
		}
		sc.doHookSync(syncTasks, hooks)
	} else {
		sc.setOperationPhase(appv1.OperationFailed, "Unknown sync strategy")
		return
	}
}

func (sc *syncContext) forceAppRefresh() {
	sc.comparison.ComparedAt = metav1.Time{}
}

// generateSyncTasks() generates the list of sync tasks we will be performing during this sync.
func (sc *syncContext) generateSyncTasks() ([]syncTask, bool) {
	syncTasks := make([]syncTask, 0)
	for _, resourceState := range sc.resources {
		liveObj, err := resourceState.LiveObject()
		if err != nil {
			sc.setOperationPhase(appv1.OperationError, fmt.Sprintf("Failed to unmarshal live object: %v", err))
			return nil, false
		}
		targetObj, err := resourceState.TargetObject()
		if err != nil {
			sc.setOperationPhase(appv1.OperationError, fmt.Sprintf("Failed to unmarshal target object: %v", err))
			return nil, false
		}
		if sc.syncResources == nil ||
			(liveObj != nil && argo.ContainsSyncResource(liveObj.GetName(), liveObj.GroupVersionKind(), sc.syncResources)) ||
			(targetObj != nil && argo.ContainsSyncResource(targetObj.GetName(), targetObj.GroupVersionKind(), sc.syncResources)) {

			syncTask := syncTask{
				liveObj:   liveObj,
				targetObj: targetObj,
			}
			syncTasks = append(syncTasks, syncTask)
		}
	}

	sort.Sort(newKindSorter(syncTasks, resourceOrder))
	return syncTasks, true
}

// startedPreSyncPhase detects if we already started the PreSync stage of a sync operation.
// This is equal to if we have anything in our resource or hook list
func (sc *syncContext) startedPreSyncPhase() bool {
	if len(sc.syncRes.Resources) > 0 {
		return true
	}
	if len(sc.syncRes.Hooks) > 0 {
		return true
	}
	return false
}

// startedSyncPhase detects if we have already started the Sync stage of a sync operation.
// This is equal to if the resource list is non-empty, or we we see Sync/PostSync hooks
func (sc *syncContext) startedSyncPhase() bool {
	if len(sc.syncRes.Resources) > 0 {
		return true
	}
	for _, hookStatus := range sc.syncRes.Hooks {
		if hookStatus.Type == appv1.HookTypeSync || hookStatus.Type == appv1.HookTypePostSync {
			return true
		}
	}
	return false
}

// startedPostSyncPhase detects if we have already started the PostSync stage. This is equal to if
// we see any PostSync hooks
func (sc *syncContext) startedPostSyncPhase() bool {
	for _, hookStatus := range sc.syncRes.Hooks {
		if hookStatus.Type == appv1.HookTypePostSync {
			return true
		}
	}
	return false
}

func (sc *syncContext) setOperationPhase(phase appv1.OperationPhase, message string) {
	if sc.opState.Phase != phase || sc.opState.Message != message {
		sc.log.Infof("Updating operation state. phase: %s -> %s, message: '%s' -> '%s'", sc.opState.Phase, phase, sc.opState.Message, message)
	}
	sc.opState.Phase = phase
	sc.opState.Message = message
}

// applyObject performs a `kubectl apply` of a single resource
func (sc *syncContext) applyObject(targetObj *unstructured.Unstructured, dryRun bool, force bool) appv1.ResourceDetails {
	resDetails := appv1.ResourceDetails{
		Name:      targetObj.GetName(),
		Kind:      targetObj.GetKind(),
		Namespace: sc.namespace,
	}
	message, err := sc.kubectl.ApplyResource(sc.config, targetObj, sc.namespace, dryRun, force)
	if err != nil {
		resDetails.Message = err.Error()
		resDetails.Status = appv1.ResourceDetailsSyncFailed
		return resDetails
	}

	resDetails.Message = message
	resDetails.Status = appv1.ResourceDetailsSynced
	return resDetails
}

// pruneObject deletes the object if both prune is true and dryRun is false. Otherwise appropriate message
func (sc *syncContext) pruneObject(liveObj *unstructured.Unstructured, prune, dryRun bool) appv1.ResourceDetails {
	resDetails := appv1.ResourceDetails{
		Name:      liveObj.GetName(),
		Kind:      liveObj.GetKind(),
		Namespace: liveObj.GetNamespace(),
	}
	if prune {
		if dryRun {
			resDetails.Message = "pruned (dry run)"
			resDetails.Status = appv1.ResourceDetailsSyncedAndPruned
		} else {
			err := sc.kubectl.DeleteResource(sc.config, liveObj, sc.namespace)
			if err != nil {
				resDetails.Message = err.Error()
				resDetails.Status = appv1.ResourceDetailsSyncFailed
			} else {
				resDetails.Message = "pruned"
				resDetails.Status = appv1.ResourceDetailsSyncedAndPruned
			}
		}
	} else {
		resDetails.Message = "ignored (requires pruning)"
		resDetails.Status = appv1.ResourceDetailsPruningRequired
	}
	return resDetails
}

func hasCRDOfGroupKind(tasks []syncTask, group, kind string) bool {
	for _, task := range tasks {
		if kube.IsCRD(task.targetObj) {
			crdGroup, ok, err := unstructured.NestedString(task.targetObj.Object, "spec", "group")
			if err != nil || !ok {
				continue
			}
			crdKind, ok, err := unstructured.NestedString(task.targetObj.Object, "spec", "names", "kind")
			if err != nil || !ok {
				continue
			}
			if group == crdGroup && crdKind == kind {
				return true
			}
		}
	}
	return false
}

// performs a apply based sync of the given sync tasks (possibly pruning the objects).
// If update is true, will updates the resource details with the result.
// Or if the prune/apply failed, will also update the result.
func (sc *syncContext) doApplySync(syncTasks []syncTask, dryRun, force, update bool) bool {
	syncSuccessful := true

	var createTasks []syncTask
	var pruneTasks []syncTask
	for _, syncTask := range syncTasks {
		if syncTask.targetObj == nil {
			pruneTasks = append(pruneTasks, syncTask)
		} else {
			createTasks = append(createTasks, syncTask)
		}
	}

	var wg sync.WaitGroup
	for _, task := range pruneTasks {
		wg.Add(1)
		go func(t syncTask) {
			defer wg.Done()
			var resDetails appv1.ResourceDetails
			resDetails = sc.pruneObject(t.liveObj, sc.syncOp.Prune, dryRun)
			if !resDetails.Status.Successful() {
				syncSuccessful = false
			}
			if update || !resDetails.Status.Successful() {
				sc.setResourceDetails(&resDetails)
			}
		}(task)
	}
	wg.Wait()

	processCreateTasks := func(tasks []syncTask, gvk schema.GroupVersionKind) {
		serverRes, err := kube.ServerResourceForGroupVersionKind(sc.disco, gvk)
		if err != nil {
			// Special case for custom resources: if custom resource definition is not supported by the cluster by defined in application then
			// skip verification using `kubectl apply --dry-run` and since CRD should be created during app synchronization.
			if dryRun && apierr.IsNotFound(err) && hasCRDOfGroupKind(createTasks, gvk.Group, gvk.Kind) {
				return
			}
			syncSuccessful = false
			for _, task := range tasks {
				sc.setResourceDetails(&appv1.ResourceDetails{
					Name:      task.targetObj.GetName(),
					Kind:      task.targetObj.GetKind(),
					Namespace: sc.namespace,
					Message:   err.Error(),
					Status:    appv1.ResourceDetailsSyncFailed,
				})
			}
			return
		}

		if !sc.proj.IsResourcePermitted(metav1.GroupKind{Group: gvk.Group, Kind: gvk.Kind}, serverRes.Namespaced) {
			syncSuccessful = false
			for _, task := range tasks {
				sc.setResourceDetails(&appv1.ResourceDetails{
					Name:      task.targetObj.GetName(),
					Kind:      task.targetObj.GetKind(),
					Namespace: sc.namespace,
					Message:   fmt.Sprintf("Resource %s:%s is not permitted in project %s.", gvk.Group, gvk.Kind, sc.proj.Name),
					Status:    appv1.ResourceDetailsSyncFailed,
				})
			}
			return
		}

		var createWg sync.WaitGroup
		for i := range tasks {
			createWg.Add(1)
			go func(t syncTask) {
				defer createWg.Done()
				if isHook(t.targetObj) {
					return
				}
				resDetails := sc.applyObject(t.targetObj, dryRun, force)
				if !resDetails.Status.Successful() {
					syncSuccessful = false
				}
				if update || !resDetails.Status.Successful() {
					sc.setResourceDetails(&resDetails)
				}
			}(tasks[i])
		}
		createWg.Wait()
	}

	var tasksGroup []syncTask
	for _, task := range createTasks {
		//Only wait if the type of the next task is different than the previous type
		if len(tasksGroup) > 0 && tasksGroup[0].targetObj.GetKind() != task.targetObj.GetKind() {
			processCreateTasks(tasksGroup, tasksGroup[0].targetObj.GroupVersionKind())
			tasksGroup = []syncTask{task}
		} else {
			tasksGroup = append(tasksGroup, task)
		}
	}
	if len(tasksGroup) > 0 {
		processCreateTasks(tasksGroup, tasksGroup[0].targetObj.GroupVersionKind())
	}
	return syncSuccessful
}

// setResourceDetails sets a resource details in the SyncResult.Resources list
func (sc *syncContext) setResourceDetails(details *appv1.ResourceDetails) {
	sc.lock.Lock()
	defer sc.lock.Unlock()
	for i, res := range sc.syncRes.Resources {
		if res.Kind == details.Kind && res.Name == details.Name {
			// update existing value
			if res.Status != details.Status {
				sc.log.Infof("updated resource %s/%s status: %s -> %s", res.Kind, res.Name, res.Status, details.Status)
			}
			if res.Message != details.Message {
				sc.log.Infof("updated resource %s/%s message: %s -> %s", res.Kind, res.Name, res.Message, details.Message)
			}
			sc.syncRes.Resources[i] = details
			return
		}
	}
	sc.log.Infof("added resource %s/%s status: %s, message: %s", details.Kind, details.Name, details.Status, details.Message)
	sc.syncRes.Resources = append(sc.syncRes.Resources, details)
}

// This code is mostly taken from https://github.com/helm/helm/blob/release-2.10/pkg/tiller/kind_sorter.go

// sortOrder is an ordering of Kinds.
type sortOrder []string

// resourceOrder represents the correct order of Kubernetes resources within a manifest
var resourceOrder sortOrder = []string{
	"Namespace",
	"ResourceQuota",
	"LimitRange",
	"PodSecurityPolicy",
	"Secret",
	"ConfigMap",
	"StorageClass",
	"PersistentVolume",
	"PersistentVolumeClaim",
	"ServiceAccount",
	"CustomResourceDefinition",
	"ClusterRole",
	"ClusterRoleBinding",
	"Role",
	"RoleBinding",
	"Service",
	"DaemonSet",
	"Pod",
	"ReplicationController",
	"ReplicaSet",
	"Deployment",
	"StatefulSet",
	"Job",
	"CronJob",
	"Ingress",
	"APIService",
}

type kindSorter struct {
	ordering  map[string]int
	manifests []syncTask
}

func newKindSorter(m []syncTask, s sortOrder) *kindSorter {
	o := make(map[string]int, len(s))
	for v, k := range s {
		o[k] = v
	}

	return &kindSorter{
		manifests: m,
		ordering:  o,
	}
}

func (k *kindSorter) Len() int { return len(k.manifests) }

func (k *kindSorter) Swap(i, j int) { k.manifests[i], k.manifests[j] = k.manifests[j], k.manifests[i] }

func (k *kindSorter) Less(i, j int) bool {
	a := k.manifests[i].targetObj
	if a == nil {
		return false
	}
	b := k.manifests[j].targetObj
	if b == nil {
		return true
	}
	first, aok := k.ordering[a.GetKind()]
	second, bok := k.ordering[b.GetKind()]
	// if same kind (including unknown) sub sort alphanumeric
	if first == second {
		// if both are unknown and of different kind sort by kind alphabetically
		if !aok && !bok && a.GetKind() != b.GetKind() {
			return a.GetKind() < b.GetKind()
		}
		return a.GetName() < b.GetName()
	}
	// unknown kind is last
	if !aok {
		return false
	}
	if !bok {
		return true
	}
	// sort different kinds
	return first < second
}
