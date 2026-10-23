/*
Copyright 2026.

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

package controller

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

const testProvisioningDelay = 10 * time.Second

// harness drives every reconciler by hand against envtest, with a fake clock,
// so that scenarios spanning hours of leases run deterministically and fast.
// The reconcilers talk to the API server directly (no cache).
type harness struct {
	clock  *clocktesting.FakeClock
	stacks *StackReconciler
	expiry *StackExpiryReconciler
	pools  *StackPoolReconciler
	claims *StackClaimReconciler
}

func newHarness() *harness {
	clk := clocktesting.NewFakeClock(time.Now().Truncate(time.Second))
	recorder := &events.FakeRecorder{}
	return &harness{
		clock: clk,
		stacks: &StackReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: clk, ProvisioningDelay: testProvisioningDelay,
		},
		expiry: &StackExpiryReconciler{Client: k8sClient, Clock: clk, Recorder: recorder},
		pools:  newPoolReconciler(clk),
		claims: &StackClaimReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: clk, Recorder: recorder},
	}
}

func newPoolReconciler(clk *clocktesting.FakeClock) *StackPoolReconciler {
	return &StackPoolReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(), Clock: clk, Recorder: &events.FakeRecorder{},
		FailureThreshold: 3, FailureBackoff: 30 * time.Second, MaxFailureBackoff: 5 * time.Minute,
	}
}

// settle reconciles every object a few times over, which is enough for the
// controllers to react to each other's writes.
func (h *harness) settle() {
	GinkgoHelper()
	for range 4 {
		for _, stack := range list(&platformv1.StackList{}).Items {
			h.reconcile(h.stacks, stack.Name)
			h.reconcile(h.expiry, stack.Name)
		}
		for _, pool := range list(&platformv1.StackPoolList{}).Items {
			h.reconcile(h.pools, pool.Name)
		}
		for _, claim := range list(&platformv1.StackClaimList{}).Items {
			h.reconcile(h.claims, claim.Name)
		}
	}
}

// provision lets simulated provisioning finish.
func (h *harness) provision() {
	GinkgoHelper()
	h.settle()
	h.clock.Step(testProvisioningDelay)
	h.settle()
}

func (h *harness) reconcile(r reconcile.Reconciler, name string) {
	GinkgoHelper()
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	Expect(err).NotTo(HaveOccurred())
}

func list[L client.ObjectList](l L) L {
	GinkgoHelper()
	Expect(k8sClient.List(ctx, l)).To(Succeed())
	return l
}

func get[O client.Object](name string, obj O) O {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, obj)).To(Succeed())
	return obj
}

func exists(name string, obj client.Object) bool {
	GinkgoHelper()
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, obj)
	if apierrors.IsNotFound(err) {
		return false
	}
	Expect(err).NotTo(HaveOccurred())
	return true
}

// update re-reads obj, applies mutate and writes it back.
func update[O client.Object](obj O, mutate func(O)) {
	GinkgoHelper()
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)).To(Succeed())
	mutate(obj)
	Expect(k8sClient.Update(ctx, obj)).To(Succeed())
}

var poolSeq int

func createPool(warm, maxStacks int32, mutate ...func(*platformv1.StackPool)) *platformv1.StackPool {
	GinkgoHelper()
	poolSeq++
	pool := &platformv1.StackPool{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pool-%d", poolSeq)},
		Spec: platformv1.StackPoolSpec{
			Warm: warm,
			Max:  maxStacks,
			Template: platformv1.StackTemplateSpec{
				Metadata: platformv1.StackTemplateMetadata{Labels: map[string]string{"team": "ci"}},
				Spec:     platformv1.StackSpec{Foo: new("bar")},
			},
		},
	}
	for _, m := range mutate {
		m(pool)
	}
	Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	return pool
}

func createClaim(name, poolName string, ttl ...time.Duration) *platformv1.StackClaim {
	GinkgoHelper()
	claim := &platformv1.StackClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       platformv1.StackClaimSpec{PoolName: poolName},
	}
	if len(ttl) > 0 {
		claim.Spec.TTL = &metav1.Duration{Duration: ttl[0]}
	}
	Expect(k8sClient.Create(ctx, claim)).To(Succeed())
	return claim
}

// stacksOf returns the live (not terminating) Stacks of a pool, oldest first.
func stacksOf(pool *platformv1.StackPool) []platformv1.Stack {
	GinkgoHelper()
	var l platformv1.StackList
	Expect(k8sClient.List(ctx, &l, client.MatchingLabels{platformv1.LabelPool: pool.Name})).To(Succeed())
	live := l.Items[:0]
	for _, s := range l.Items {
		if s.DeletionTimestamp.IsZero() {
			live = append(live, s)
		}
	}
	slices.SortFunc(live, func(a, b platformv1.Stack) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.Name, b.Name))
	})
	return live
}

func unclaimed(stacks []platformv1.Stack) []platformv1.Stack {
	var out []platformv1.Stack
	for _, s := range stacks {
		if s.Spec.ClaimRef == nil {
			out = append(out, s)
		}
	}
	return out
}

// cleanup deletes every object the tests created. envtest runs no garbage
// collector, so Stacks are deleted explicitly.
func cleanup() {
	GinkgoHelper()
	for _, claim := range list(&platformv1.StackClaimList{}).Items {
		if controllerutil.RemoveFinalizer(&claim, claimFinalizer) {
			Expect(client.IgnoreNotFound(k8sClient.Update(ctx, &claim))).To(Succeed())
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &claim))).To(Succeed())
	}
	Expect(k8sClient.DeleteAllOf(ctx, &platformv1.Stack{})).To(Succeed())
	Expect(k8sClient.DeleteAllOf(ctx, &platformv1.StackPool{})).To(Succeed())
}
