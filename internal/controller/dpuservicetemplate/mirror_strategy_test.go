/*
Copyright 2025.

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

package dpuservicetemplate

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	imagereference "github.com/openshift/library-go/pkg/image/reference"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newMirrorTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	Expect(configv1.Install(scheme)).To(Succeed())
	return scheme
}

var _ = Describe("Mirror Strategy", func() {
	Describe("clusterIDMSStrategy", func() {
		It("should return nil when no IDMS resources exist", func() {
			c := fake.NewClientBuilder().WithScheme(newMirrorTestScheme()).Build()
			s := &clusterIDMSStrategy{client: c}

			ref, _ := imagereference.Parse("quay.io/openshift-release-dev/ocp-release:4.19.0-aarch64")
			alternates, err := s.FirstRequest(context.TODO(), ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(alternates).To(BeNil())
		})

		It("should return mirrors when IDMS matches", func() {
			idms := &configv1.ImageDigestMirrorSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-mirror"},
				Spec: configv1.ImageDigestMirrorSetSpec{
					ImageDigestMirrors: []configv1.ImageDigestMirrors{
						{
							Source:  "quay.io/openshift-release-dev",
							Mirrors: []configv1.ImageMirror{"mirror.example.com/openshift"},
						},
					},
				},
			}
			c := fake.NewClientBuilder().WithScheme(newMirrorTestScheme()).WithObjects(idms).Build()
			s := &clusterIDMSStrategy{client: c}

			ref, _ := imagereference.Parse("quay.io/openshift-release-dev/ocp-release:4.19.0-aarch64")
			alternates, err := s.FirstRequest(context.TODO(), ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(alternates).To(HaveLen(2))
			Expect(alternates[0].Exact()).To(ContainSubstring("mirror.example.com"))
			Expect(alternates[1].Exact()).To(ContainSubstring("quay.io"))
		})

		It("should omit source when NeverContactSource is set", func() {
			idms := &configv1.ImageDigestMirrorSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-mirror"},
				Spec: configv1.ImageDigestMirrorSetSpec{
					ImageDigestMirrors: []configv1.ImageDigestMirrors{
						{
							Source:             "quay.io/openshift-release-dev",
							Mirrors:            []configv1.ImageMirror{"mirror.example.com/openshift"},
							MirrorSourcePolicy: configv1.NeverContactSource,
						},
					},
				},
			}
			c := fake.NewClientBuilder().WithScheme(newMirrorTestScheme()).WithObjects(idms).Build()
			s := &clusterIDMSStrategy{client: c}

			ref, _ := imagereference.Parse("quay.io/openshift-release-dev/ocp-release:4.19.0-aarch64")
			alternates, err := s.FirstRequest(context.TODO(), ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(alternates).To(HaveLen(1))
			Expect(alternates[0].Exact()).To(ContainSubstring("mirror.example.com"))
		})

		It("should return nil when IDMS source doesn't match", func() {
			idms := &configv1.ImageDigestMirrorSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-mirror"},
				Spec: configv1.ImageDigestMirrorSetSpec{
					ImageDigestMirrors: []configv1.ImageDigestMirrors{
						{
							Source:  "quay.io/other-org",
							Mirrors: []configv1.ImageMirror{"mirror.example.com/other"},
						},
					},
				},
			}
			c := fake.NewClientBuilder().WithScheme(newMirrorTestScheme()).WithObjects(idms).Build()
			s := &clusterIDMSStrategy{client: c}

			ref, _ := imagereference.Parse("quay.io/openshift-release-dev/ocp-release:4.19.0-aarch64")
			alternates, err := s.FirstRequest(context.TODO(), ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(alternates).To(BeNil())
		})

		It("should handle subrepo matching", func() {
			idms := &configv1.ImageDigestMirrorSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-mirror"},
				Spec: configv1.ImageDigestMirrorSetSpec{
					ImageDigestMirrors: []configv1.ImageDigestMirrors{
						{
							Source:  "quay.io/openshift-release-dev",
							Mirrors: []configv1.ImageMirror{"mirror.example.com/ocp"},
						},
					},
				},
			}
			c := fake.NewClientBuilder().WithScheme(newMirrorTestScheme()).WithObjects(idms).Build()
			s := &clusterIDMSStrategy{client: c}

			ref, _ := imagereference.Parse("quay.io/openshift-release-dev/ocp-release:4.19.0-aarch64")
			alternates, err := s.FirstRequest(context.TODO(), ref)
			Expect(err).NotTo(HaveOccurred())
			Expect(alternates).NotTo(BeNil())
			Expect(alternates[0].Exact()).To(ContainSubstring("mirror.example.com/ocp/ocp-release"))
		})
	})

	Describe("isSubrepo", func() {
		It("should match exact repo", func() {
			Expect(isSubrepo("quay.io/foo", "quay.io/foo")).To(BeTrue())
		})

		It("should match subrepo", func() {
			Expect(isSubrepo("quay.io/foo/bar", "quay.io/foo")).To(BeTrue())
		})

		It("should not match prefix without slash boundary", func() {
			Expect(isSubrepo("quay.io/foobar", "quay.io/foo")).To(BeFalse())
		})

		It("should not match shorter string", func() {
			Expect(isSubrepo("quay.io/fo", "quay.io/foo")).To(BeFalse())
		})
	})
})
