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
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	platformv1 "github.com/samueltorres/stack-operator/api/v1"
)

var _ = Describe("API validation", func() {
	AfterEach(cleanup)

	DescribeTable("accepts the samples",
		func(file string) {
			data, err := os.ReadFile(filepath.Join("..", "..", "config", "samples", file))
			Expect(err).NotTo(HaveOccurred())
			obj := &unstructured.Unstructured{}
			Expect(yaml.Unmarshal(data, &obj.Object)).To(Succeed())
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		},
		Entry(nil, "platform_v1_stack.yaml"),
		Entry(nil, "platform_v1_stackpool.yaml"),
		Entry(nil, "platform_v1_stackclaim.yaml"),
	)

	It("accepts the examples", func() {
		files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).NotTo(BeEmpty())

		for _, file := range files {
			f, err := os.Open(file)
			Expect(err).NotTo(HaveOccurred())
			decoder := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
			for {
				obj := &unstructured.Unstructured{}
				if err := decoder.Decode(&obj.Object); errors.Is(err, io.EOF) {
					break
				} else {
					Expect(err).NotTo(HaveOccurred(), file)
				}
				Expect(k8sClient.Create(ctx, obj)).To(Succeed(), file)
			}
			Expect(f.Close()).To(Succeed())
		}
	})

	It("defaults a pool's defaultTTL to two hours", func() {
		pool := createPool(0, 1)
		Expect(get(pool.Name, pool).Spec.DefaultTTL.Duration).To(Equal(2 * time.Hour))
	})

	DescribeTable("rejects invalid pools",
		func(mutate func(*platformv1.StackPool), message string) {
			pool := &platformv1.StackPool{
				ObjectMeta: metav1.ObjectMeta{Name: "invalid"},
				Spec:       platformv1.StackPoolSpec{Warm: 1, Max: 2},
			}
			mutate(pool)
			Expect(k8sClient.Create(ctx, pool)).To(MatchError(ContainSubstring(message)))
		},
		Entry("warm above max", func(p *platformv1.StackPool) { p.Spec.Warm = 3 }, "warm must not exceed max"),
		Entry("defaultTTL above maxTTL", func(p *platformv1.StackPool) {
			p.Spec.MaxTTL = &metav1.Duration{Duration: time.Hour}
		}, "defaultTTL must not exceed maxTTL"),
		Entry("templated claimRef", func(p *platformv1.StackPool) {
			p.Spec.Template.Spec.ClaimRef = &platformv1.ClaimReference{Name: "c", UID: "u"}
		}, "cannot be templated"),
	)
})
