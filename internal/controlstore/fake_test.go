package controlstore

import (
	"errors"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	testNamespace = "kelson-system"
	testProject   = "shop"
	testEnv       = "production"
)

// newFakeClient returns a fake clientset with the one behaviour the stores
// depend on and the fixture omits: resourceVersion.
//
// The upstream fake tracker stores whatever resourceVersion it is handed and
// never assigns or bumps one, so every object reads back at "" and no update
// ever conflicts. Optimistic concurrency is the whole point of the spec store
// (ADR-0013 §1), so the fake is given the API server's contract for it —
// monotonic versions on create and update, a 409 on a stale one — and nothing
// else is changed.
func newFakeClient(objs ...runtime.Object) *fake.Clientset {
	c := fake.NewSimpleClientset(objs...)
	var mu sync.Mutex
	var seq int64
	next := func() string {
		mu.Lock()
		defer mu.Unlock()
		seq++
		return strconv.FormatInt(seq, 10)
	}

	c.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(k8stesting.CreateActionImpl)
		if !ok {
			return false, nil, nil
		}
		cm, ok := ca.GetObject().(*corev1.ConfigMap)
		if !ok {
			return false, nil, nil
		}
		if _, err := c.Tracker().Get(ca.GetResource(), ca.GetNamespace(), cm.Name); err == nil {
			return true, nil, apierrors.NewAlreadyExists(ca.GetResource().GroupResource(), cm.Name)
		}
		stored := cm.DeepCopy()
		stored.ResourceVersion = next()
		if err := c.Tracker().Create(ca.GetResource(), stored, ca.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, stored.DeepCopy(), nil
	})

	c.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(k8stesting.UpdateActionImpl)
		if !ok {
			return false, nil, nil
		}
		cm, ok := ua.GetObject().(*corev1.ConfigMap)
		if !ok {
			return false, nil, nil
		}
		obj, err := c.Tracker().Get(ua.GetResource(), ua.GetNamespace(), cm.Name)
		if err != nil {
			return true, nil, err
		}
		live, ok := obj.(*corev1.ConfigMap)
		if !ok {
			return true, nil, errors.New("tracker returned a non-ConfigMap")
		}
		if cm.ResourceVersion != "" && cm.ResourceVersion != live.ResourceVersion {
			return true, nil, apierrors.NewConflict(ua.GetResource().GroupResource(), cm.Name,
				errors.New("the object has been modified; please apply your changes to the latest version and try again"))
		}
		stored := cm.DeepCopy()
		stored.ResourceVersion = next()
		if err := c.Tracker().Update(ua.GetResource(), stored, ua.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, stored.DeepCopy(), nil
	})

	return c
}
