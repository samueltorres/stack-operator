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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

var _ = Describe("expectations", func() {
	var (
		clk *clocktesting.FakeClock
		e   *expectations
	)
	stack := func(name string, uid types.UID) platformv1.Stack {
		return platformv1.Stack{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid}}
	}

	BeforeEach(func() {
		clk = clocktesting.NewFakeClock(time.Now())
		e = newExpectations(clk)
	})

	It("is satisfied when nothing is expected", func() {
		Expect(e.satisfied("pool", nil)).To(BeTrue())
	})

	It("waits until a created Stack shows up", func() {
		e.expectCreate("pool", "s1")
		Expect(e.satisfied("pool", nil)).To(BeFalse())
		Expect(e.satisfied("pool", []platformv1.Stack{stack("s1", "u1")})).To(BeTrue())
	})

	It("waits until a deleted Stack is terminating or gone", func() {
		live := stack("s1", "u1")
		e.expectDelete("pool", "u1")
		Expect(e.satisfied("pool", []platformv1.Stack{live})).To(BeFalse())

		live.DeletionTimestamp = &metav1.Time{Time: clk.Now()}
		Expect(e.satisfied("pool", []platformv1.Stack{live})).To(BeTrue())

		e.expectDelete("pool", "u1")
		Expect(e.satisfied("pool", nil)).To(BeTrue())
	})

	It("gives up on mutations that are never observed", func() {
		e.expectCreate("pool", "s1")
		clk.Step(expectationsTimeout + time.Second)
		Expect(e.satisfied("pool", nil)).To(BeTrue())
	})

	It("keeps pools apart", func() {
		e.expectCreate("a", "s1")
		Expect(e.satisfied("b", nil)).To(BeTrue())
		e.forget("a")
		Expect(e.satisfied("a", nil)).To(BeTrue())
	})
})
