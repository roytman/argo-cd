package diff

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/ghodss/yaml"
	"github.com/stretchr/testify/assert"
	"github.com/yudai/gojsondiff/formatter"
	"golang.org/x/crypto/ssh/terminal"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/argoproj/argo-cd/test"
	"github.com/argoproj/argo-cd/util/kube"
)

var (
	formatOpts = formatter.AsciiFormatterConfig{
		Coloring: terminal.IsTerminal(int(os.Stdout.Fd())),
	}
)

func TestDiff(t *testing.T) {
	leftDep := test.DemoDeployment()
	leftUn := kube.MustToUnstructured(leftDep)

	diffRes := Diff(leftUn, leftUn)
	assert.False(t, diffRes.Diff.Modified())
	ascii, err := diffRes.ASCIIFormat(leftUn, formatOpts)
	assert.Nil(t, err)
	if ascii != "" {
		log.Println(ascii)
	}
}

func TestDiffWithNils(t *testing.T) {
	dep := test.DemoDeployment()
	resource := kube.MustToUnstructured(dep)

	diffRes := Diff(nil, resource)
	// NOTE: if live is non-nil, and config is nil, this is not considered difference
	// This "difference" is checked at the comparator.
	assert.False(t, diffRes.Diff.Modified())
	diffRes = TwoWayDiff(nil, resource)
	assert.False(t, diffRes.Diff.Modified())

	diffRes = Diff(resource, nil)
	assert.True(t, diffRes.Diff.Modified())
	diffRes = TwoWayDiff(resource, nil)
	assert.True(t, diffRes.Diff.Modified())
}

func TestDiffArraySame(t *testing.T) {
	leftDep := test.DemoDeployment()
	rightDep := leftDep.DeepCopy()

	leftUn := kube.MustToUnstructured(leftDep)
	rightUn := kube.MustToUnstructured(rightDep)

	left := []*unstructured.Unstructured{leftUn}
	right := []*unstructured.Unstructured{rightUn}
	diffResList, err := DiffArray(left, right)
	assert.Nil(t, err)
	assert.False(t, diffResList.Modified)
}

func TestDiffArrayAdditions(t *testing.T) {
	leftDep := test.DemoDeployment()
	rightDep := leftDep.DeepCopy()
	rightDep.Status.Replicas = 1

	leftUn := kube.MustToUnstructured(leftDep)
	rightUn := kube.MustToUnstructured(rightDep)

	left := []*unstructured.Unstructured{leftUn}
	right := []*unstructured.Unstructured{rightUn}
	diffResList, err := DiffArray(left, right)
	assert.Nil(t, err)
	assert.False(t, diffResList.Modified)
}

func TestDiffArrayModification(t *testing.T) {
	leftDep := test.DemoDeployment()
	rightDep := leftDep.DeepCopy()
	ten := int32(10)
	rightDep.Spec.Replicas = &ten

	leftUn := kube.MustToUnstructured(leftDep)
	rightUn := kube.MustToUnstructured(rightDep)

	left := []*unstructured.Unstructured{leftUn}
	right := []*unstructured.Unstructured{rightUn}
	diffResList, err := DiffArray(left, right)
	assert.Nil(t, err)
	assert.True(t, diffResList.Modified)
}

// TestThreeWayDiff will perform a diff when there is a kubectl.kubernetes.io/last-applied-configuration
// present in the live object.
func TestThreeWayDiff(t *testing.T) {
	// 1. get config and live to be the same. Both have a foo annotation.
	configDep := test.DemoDeployment()
	configDep.ObjectMeta.Namespace = ""
	configDep.Annotations = map[string]string{
		"foo": "bar",
	}
	liveDep := configDep.DeepCopy()

	// 2. add a extra field to the live. this simulates kubernetes adding default values in the
	// object. We should not consider defaulted values as a difference
	liveDep.SetNamespace("default")
	configUn := kube.MustToUnstructured(configDep)
	liveUn := kube.MustToUnstructured(liveDep)
	res := Diff(configUn, liveUn)
	if !assert.False(t, res.Modified) {
		ascii, err := res.ASCIIFormat(liveUn, formatOpts)
		assert.Nil(t, err)
		log.Println(ascii)
	}

	// 3. Add a last-applied-configuration annotation in the live. There should still not be any
	// difference
	configBytes, err := json.Marshal(configDep)
	assert.Nil(t, err)
	liveDep.Annotations[v1.LastAppliedConfigAnnotation] = string(configBytes)
	configUn = kube.MustToUnstructured(configDep)
	liveUn = kube.MustToUnstructured(liveDep)
	res = Diff(configUn, liveUn)
	if !assert.False(t, res.Modified) {
		ascii, err := res.ASCIIFormat(liveUn, formatOpts)
		assert.Nil(t, err)
		log.Println(ascii)
	}

	// 4. Remove the foo annotation from config and perform the diff again. We should detect a
	// difference since three-way diff detects the removal of a managed field
	delete(configDep.Annotations, "foo")
	configUn = kube.MustToUnstructured(configDep)
	liveUn = kube.MustToUnstructured(liveDep)
	res = Diff(configUn, liveUn)
	assert.True(t, res.Modified)

	// 5. Just to prove three way diff incorporates last-applied-configuration, remove the
	// last-applied-configuration annotation from the live object, and redo the diff. This time,
	// the diff will report not modified (because we have no way of knowing what was a defaulted
	// field without this annotation)
	delete(liveDep.Annotations, v1.LastAppliedConfigAnnotation)
	configUn = kube.MustToUnstructured(configDep)
	liveUn = kube.MustToUnstructured(liveDep)
	res = Diff(configUn, liveUn)
	ascii, err := res.ASCIIFormat(liveUn, formatOpts)
	assert.Nil(t, err)
	if ascii != "" {
		log.Println(ascii)
	}
	assert.False(t, res.Modified)
}

var demoConfig = `
{
  "apiVersion": "v1",
  "kind": "ServiceAccount",
  "metadata": {
    "labels": {
      "applications.argoproj.io/app-name": "argocd-demo"
    },
    "name": "application-controller"
  }
}
`

var demoLive = `
{
  "apiVersion": "v1",
  "kind": "ServiceAccount",
  "metadata": {
    "annotations": {
      "kubectl.kubernetes.io/last-applied-configuration": "{\"apiVersion\":\"v1\",\"kind\":\"ServiceAccount\",\"metadata\":{\"annotations\":{},\"labels\":{\"applications.argoproj.io/app-name\":\"argocd-demo\"},\"name\":\"application-controller\",\"namespace\":\"argocd-demo\"}}\n"
    },
    "creationTimestamp": "2018-04-16T22:08:57Z",
    "labels": {
      "applications.argoproj.io/app-name": "argocd-demo"
    },
    "name": "application-controller",
    "namespace": "argocd-demo",
    "resourceVersion": "7584502",
    "selfLink": "/api/v1/namespaces/argocd-demo/serviceaccounts/application-controller",
    "uid": "c22bb2b4-41c2-11e8-978a-028445d52ec8"
  },
  "secrets": [
    {
      "name": "application-controller-token-kfxct"
    }
  ]
}
`

// Tests a real world example
func TestThreeWayDiffExample1(t *testing.T) {
	var configUn, liveUn unstructured.Unstructured
	// NOTE: it is intentional to unmarshal to Unstructured.Object instead of just Unstructured
	// since it catches a case when we comparison fails due to subtle differences in types
	// (e.g. float vs. int)
	err := json.Unmarshal([]byte(demoConfig), &configUn.Object)
	assert.Nil(t, err)
	err = json.Unmarshal([]byte(demoLive), &liveUn.Object)
	assert.Nil(t, err)
	dr := Diff(&configUn, &liveUn)
	assert.False(t, dr.Modified)
	ascii, err := dr.ASCIIFormat(&liveUn, formatOpts)
	assert.Nil(t, err)
	if ascii != "" {
		log.Println(ascii)
	}

}

func TestThreeWayDiffExample2(t *testing.T) {
	configData, err := ioutil.ReadFile("testdata/elasticsearch-config.json")
	assert.NoError(t, err)
	liveData, err := ioutil.ReadFile("testdata/elasticsearch-live.json")
	assert.NoError(t, err)
	var configUn, liveUn unstructured.Unstructured
	err = json.Unmarshal(configData, &configUn.Object)
	assert.NoError(t, err)
	err = json.Unmarshal(liveData, &liveUn.Object)
	assert.NoError(t, err)
	dr := Diff(&configUn, &liveUn)
	assert.False(t, dr.Modified)
	ascii, err := dr.ASCIIFormat(&liveUn, formatOpts)
	assert.Nil(t, err)
	log.Println(ascii)
}

// TestThreeWayDiffExample2WithDifference is same as TestThreeWayDiffExample2 but with differences
func TestThreeWayDiffExample2WithDifference(t *testing.T) {
	configData, err := ioutil.ReadFile("testdata/elasticsearch-config.json")
	assert.NoError(t, err)
	liveData, err := ioutil.ReadFile("testdata/elasticsearch-live.json")
	assert.NoError(t, err)
	var configUn, liveUn unstructured.Unstructured
	err = json.Unmarshal(configData, &configUn.Object)
	assert.NoError(t, err)
	err = json.Unmarshal(liveData, &liveUn.Object)
	assert.NoError(t, err)

	labels := configUn.GetLabels()
	// add a new label
	labels["foo"] = "bar"
	// modify a label
	labels["chart"] = "elasticsearch-1.7.1"
	// remove an existing label
	delete(labels, "release")
	configUn.SetLabels(labels)

	dr := Diff(&configUn, &liveUn)
	assert.True(t, dr.Modified)
	ascii, err := dr.ASCIIFormat(&liveUn, formatOpts)
	assert.Nil(t, err)
	log.Println(ascii)

	// Check that we indicate missing/extra/changed correctly
	showsMissing := 0
	showsExtra := 0
	showsChanged := 0
	for _, line := range strings.Split(ascii, "\n") {
		if strings.HasPrefix(line, `+      "foo": "bar"`) {
			showsMissing++
		}
		if strings.HasPrefix(line, `-      "release": "elasticsearch4"`) {
			showsExtra++
		}
		if strings.HasPrefix(line, `+      "chart": "elasticsearch-1.7.1"`) {
			showsChanged++
		}
		if strings.HasPrefix(line, `-      "chart": "elasticsearch-1.7.0"`) {
			showsChanged++
		}
	}
	assert.Equal(t, 1, showsMissing)
	assert.Equal(t, 1, showsExtra)
	assert.Equal(t, 2, showsChanged)
}

func TestThreeWayDiffExplicitNamespace(t *testing.T) {
	configData, err := ioutil.ReadFile("testdata/spinnaker-sa-config.json")
	assert.NoError(t, err)
	liveData, err := ioutil.ReadFile("testdata/spinnaker-sa-live.json")
	assert.NoError(t, err)
	var configUn, liveUn unstructured.Unstructured
	err = json.Unmarshal(configData, &configUn.Object)
	assert.NoError(t, err)
	err = json.Unmarshal(liveData, &liveUn.Object)
	assert.NoError(t, err)
	dr := Diff(&configUn, &liveUn)
	assert.False(t, dr.Modified)
	ascii, err := dr.ASCIIFormat(&liveUn, formatOpts)
	assert.Nil(t, err)
	log.Println(ascii)
}

func TestRemoveNamespaceAnnotation(t *testing.T) {
	obj := removeNamespaceAnnotation(&unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      "test",
			"namespace": "default",
		},
	}})
	assert.Equal(t, "", obj.GetNamespace())

	obj = removeNamespaceAnnotation(&unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":        "test",
			"namespace":   "default",
			"annotations": make(map[string]interface{}),
		},
	}})
	assert.Equal(t, "", obj.GetNamespace())
	assert.Nil(t, obj.GetAnnotations())
}

const customObjConfig = `
apiVersion: foo.io/v1
kind: Foo
metadata:
  name: my-foo
  namespace: kube-system
spec:
  foo: bar
`

const customObjLive = `
apiVersion: foo.io/v1
kind: Foo
metadata:
  creationTimestamp: 2018-07-17 09:17:05 UTC
  name: my-foo
  resourceVersion: '10308211'
  selfLink: "/apis/rbac.authorization.k8s.io/v1/clusterroles/argocd-manager-role"
  uid: 2c3d5405-89a2-11e8-aff0-42010a8a0fc6
spec:
  foo: bar
`

func TestIgnoreNamespaceForClusterScopedResources(t *testing.T) {
	var configUn unstructured.Unstructured
	var liveUn unstructured.Unstructured
	err := yaml.Unmarshal([]byte(customObjLive), &liveUn)
	assert.Nil(t, err)
	err = yaml.Unmarshal([]byte(customObjConfig), &configUn)
	assert.Nil(t, err)
	dr := Diff(&configUn, &liveUn)
	assert.False(t, dr.Modified)
}

const secretConfig = `
apiVersion: v1
kind: Secret
metadata:
  name: my-secret
type: Opaque
stringData:
  foo: bar
  bar: "1234"
data:
  baz: cXV4
`

const secretLive = `
apiVersion: v1
kind: Secret
metadata:
  creationTimestamp: 2018-11-19T11:30:40Z
  name: my-secret
  namespace: argocd
  resourceVersion: "25848035"
  selfLink: /api/v1/namespaces/argocd/secrets/my-secret
  uid: 8b4a2766-ebee-11e8-93c0-42010a8a0013
type: Opaque
data:
  foo: YmFy
  bar: MTIzNA==
  baz: cXV4
`

func TestSecretStringData(t *testing.T) {
	var err error
	var configUn unstructured.Unstructured
	err = yaml.Unmarshal([]byte(secretConfig), &configUn)
	assert.Nil(t, err)

	var liveUn unstructured.Unstructured
	err = yaml.Unmarshal([]byte(secretLive), &liveUn)
	assert.Nil(t, err)

	dr := Diff(&configUn, &liveUn)
	if !assert.False(t, dr.Modified) {
		ascii, err := dr.ASCIIFormat(&liveUn, formatOpts)
		assert.Nil(t, err)
		log.Println(ascii)
	}
}

// This is invalid because foo is a number, not a string
const secretInvalidConfig = `
apiVersion: v1
kind: Secret
metadata:
  name: my-secret
type: Opaque
stringData:
  foo: 1234
`

const secretInvalidLive = `
apiVersion: v1
kind: Secret
metadata:
  creationTimestamp: 2018-11-19T11:30:40Z
  name: my-secret
  namespace: argocd
  resourceVersion: "25848035"
  selfLink: /api/v1/namespaces/argocd/secrets/my-secret
  uid: 8b4a2766-ebee-11e8-93c0-42010a8a0013
type: Opaque
data:
  foo: MTIzNA==
`

func TestInvalidSecretStringData(t *testing.T) {
	var err error
	var configUn unstructured.Unstructured
	err = yaml.Unmarshal([]byte(secretInvalidConfig), &configUn)
	assert.Nil(t, err)

	var liveUn unstructured.Unstructured
	err = yaml.Unmarshal([]byte(secretInvalidLive), &liveUn)
	assert.Nil(t, err)

	dr := Diff(&configUn, nil)
	assert.True(t, dr.Modified)
}
