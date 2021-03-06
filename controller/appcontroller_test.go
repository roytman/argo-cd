package controller

import (
	"testing"
	"time"

	"github.com/ghodss/yaml"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"

	argoappv1 "github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/pkg/client/clientset/versioned/fake"
	reposerver "github.com/argoproj/argo-cd/reposerver/mocks"
	"github.com/stretchr/testify/assert"
)

func newFakeController(apps ...runtime.Object) *ApplicationController {
	var clust corev1.Secret
	err := yaml.Unmarshal([]byte(fakeCluster), &clust)
	if err != nil {
		panic(err)
	}
	kubeClientset := fake.NewSimpleClientset(&clust)
	appClientset := appclientset.NewSimpleClientset(apps...)
	repoClientset := reposerver.Clientset{}
	return NewApplicationController(
		"argocd",
		kubeClientset,
		appClientset,
		&repoClientset,
		time.Minute,
	)
}

var fakeCluster = `
apiVersion: v1
data:
  # {"bearerToken":"fake","tlsClientConfig":{"insecure":true},"awsAuthConfig":null}
  config: eyJiZWFyZXJUb2tlbiI6ImZha2UiLCJ0bHNDbGllbnRDb25maWciOnsiaW5zZWN1cmUiOnRydWV9LCJhd3NBdXRoQ29uZmlnIjpudWxsfQ==
  # minikube
  name: aHR0cHM6Ly9sb2NhbGhvc3Q6NjQ0Mw==
  # https://localhost:6443
  server: aHR0cHM6Ly9rdWJlcm5ldGVzLmRlZmF1bHQuc3Zj
kind: Secret
metadata:
  creationTimestamp: 2018-11-18T23:56:59Z
  labels:
    argocd.argoproj.io/secret-type: cluster
  name: localhost-6443
  namespace: argocd
  resourceVersion: "732433"
  selfLink: /api/v1/namespaces/argocd/secrets/kubernetes.default.svc-443
  uid: a31808e1-eb8d-11e8-b3c3-9ae2f452bd03
type: Opaque
`

var fakeApp = `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: my-app
  namespace: argocd
spec:
  destination:
    namespace: dummy-namespace
    server: https://localhost:6443
  project: default
  source:
    path: some/path
    repoURL: https://github.com/argoproj/argocd-example-apps.git
  syncPolicy:
    automated: {}
status:
  operationState:
    finishedAt: 2018-09-21T23:50:29Z
    message: successfully synced
    operation:
      sync:
        revision: HEAD
    phase: Succeeded
    startedAt: 2018-09-21T23:50:25Z
    syncResult:
      resources:
      - kind: RoleBinding
        message: |-
          rolebinding.rbac.authorization.k8s.io/always-outofsync reconciled
          rolebinding.rbac.authorization.k8s.io/always-outofsync configured
        name: always-outofsync
        namespace: default
        status: Synced
      revision: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`

func newFakeApp() *argoappv1.Application {
	var app argoappv1.Application
	err := yaml.Unmarshal([]byte(fakeApp), &app)
	if err != nil {
		panic(err)
	}
	return &app
}

func TestAutoSync(t *testing.T) {
	app := newFakeApp()
	ctrl := newFakeController(app)
	compRes := argoappv1.ComparisonResult{
		Status:   argoappv1.ComparisonStatusOutOfSync,
		Revision: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	cond := ctrl.autoSync(app, &compRes)
	assert.Nil(t, cond)
	app, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications("argocd").Get("my-app", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, app.Operation)
	assert.NotNil(t, app.Operation.Sync)
	assert.False(t, app.Operation.Sync.Prune)
}

func TestSkipAutoSync(t *testing.T) {
	// Verify we skip when we previously synced to it in our most recent history
	// Set current to 'aaaaa', desired to 'aaaa' and mark system OutOfSync
	app := newFakeApp()
	ctrl := newFakeController(app)
	compRes := argoappv1.ComparisonResult{
		Status:   argoappv1.ComparisonStatusOutOfSync,
		Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	cond := ctrl.autoSync(app, &compRes)
	assert.Nil(t, cond)
	app, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications("argocd").Get("my-app", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Nil(t, app.Operation)

	// Verify we skip when we are already Synced (even if revision is different)
	app = newFakeApp()
	ctrl = newFakeController(app)
	compRes = argoappv1.ComparisonResult{
		Status:   argoappv1.ComparisonStatusSynced,
		Revision: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	cond = ctrl.autoSync(app, &compRes)
	assert.Nil(t, cond)
	app, err = ctrl.applicationClientset.ArgoprojV1alpha1().Applications("argocd").Get("my-app", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Nil(t, app.Operation)

	// Verify we skip when auto-sync is disabled
	app = newFakeApp()
	app.Spec.SyncPolicy = nil
	ctrl = newFakeController(app)
	compRes = argoappv1.ComparisonResult{
		Status:   argoappv1.ComparisonStatusOutOfSync,
		Revision: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	cond = ctrl.autoSync(app, &compRes)
	assert.Nil(t, cond)
	app, err = ctrl.applicationClientset.ArgoprojV1alpha1().Applications("argocd").Get("my-app", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Nil(t, app.Operation)

	// Verify we skip when previous sync attempt failed and return error condition
	// Set current to 'aaaaa', desired to 'bbbbb' and add 'bbbbb' to failure history
	app = newFakeApp()
	app.Status.OperationState = &argoappv1.OperationState{
		Operation: argoappv1.Operation{
			Sync: &argoappv1.SyncOperation{},
		},
		Phase: argoappv1.OperationFailed,
		SyncResult: &argoappv1.SyncOperationResult{
			Revision: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}
	ctrl = newFakeController(app)
	compRes = argoappv1.ComparisonResult{
		Status:   argoappv1.ComparisonStatusOutOfSync,
		Revision: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	cond = ctrl.autoSync(app, &compRes)
	assert.NotNil(t, cond)
	app, err = ctrl.applicationClientset.ArgoprojV1alpha1().Applications("argocd").Get("my-app", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Nil(t, app.Operation)
}

// TestAutoSyncIndicateError verifies we skip auto-sync and return error condition if previous sync failed
func TestAutoSyncIndicateError(t *testing.T) {
	app := newFakeApp()
	app.Spec.Source.ComponentParameterOverrides = []argoappv1.ComponentParameter{
		{
			Name:  "a",
			Value: "1",
		},
	}
	ctrl := newFakeController(app)
	compRes := argoappv1.ComparisonResult{
		Status:   argoappv1.ComparisonStatusOutOfSync,
		Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	app.Status.OperationState = &argoappv1.OperationState{
		Operation: argoappv1.Operation{
			Sync: &argoappv1.SyncOperation{
				ParameterOverrides: argoappv1.ParameterOverrides{
					{
						Name:  "a",
						Value: "1",
					},
				},
			},
		},
		Phase: argoappv1.OperationFailed,
		SyncResult: &argoappv1.SyncOperationResult{
			Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	cond := ctrl.autoSync(app, &compRes)
	assert.NotNil(t, cond)
	app, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications("argocd").Get("my-app", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Nil(t, app.Operation)
}

// TestAutoSyncParameterOverrides verifies we auto-sync if revision is same but parameter overrides are different
func TestAutoSyncParameterOverrides(t *testing.T) {
	app := newFakeApp()
	app.Spec.Source.ComponentParameterOverrides = []argoappv1.ComponentParameter{
		{
			Name:  "a",
			Value: "1",
		},
	}
	ctrl := newFakeController(app)
	compRes := argoappv1.ComparisonResult{
		Status:   argoappv1.ComparisonStatusOutOfSync,
		Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	app.Status.OperationState = &argoappv1.OperationState{
		Operation: argoappv1.Operation{
			Sync: &argoappv1.SyncOperation{
				ParameterOverrides: argoappv1.ParameterOverrides{
					{
						Name:  "a",
						Value: "2", // this value changed
					},
				},
			},
		},
		Phase: argoappv1.OperationFailed,
		SyncResult: &argoappv1.SyncOperationResult{
			Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	cond := ctrl.autoSync(app, &compRes)
	assert.Nil(t, cond)
	app, err := ctrl.applicationClientset.ArgoprojV1alpha1().Applications("argocd").Get("my-app", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.NotNil(t, app.Operation)
}

// TestFinalizeAppDeletion verifies application deletion
func TestFinalizeAppDeletion(t *testing.T) {
	app := newFakeApp()
	ctrl := newFakeController(app)

	fakeAppCs := ctrl.applicationClientset.(*appclientset.Clientset)
	patched := false
	fakeAppCs.ReactionChain = nil
	fakeAppCs.AddReactor("patch", "*", func(action kubetesting.Action) (handled bool, ret runtime.Object, err error) {
		patched = true
		return true, nil, nil
	})
	err := ctrl.finalizeApplicationDeletion(app)
	// TODO: use an interface to fake out the calls to GetResourcesWithLabel and DeleteResourceWithLabel
	// For now just ensure we have an expected error condition
	assert.Error(t, err)     // Change this to assert.Nil when we stub out GetResourcesWithLabel/DeleteResourceWithLabel
	assert.False(t, patched) // Change this to assert.True when we stub out GetResourcesWithLabel/DeleteResourceWithLabel

}
