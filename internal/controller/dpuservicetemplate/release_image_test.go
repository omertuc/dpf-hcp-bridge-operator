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
	"encoding/json"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Release Image Utilities", func() {
	Describe("findComponentInData", func() {
		It("should find a component by name", func() {
			refs := imageReferences{}
			refs.Spec.Tags = []struct {
				Name string `json:"name"`
				From struct {
					Name string `json:"name"`
				} `json:"from"`
			}{
				{Name: "ovn-kubernetes", From: struct {
					Name string `json:"name"`
				}{Name: "quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc123"}},
				{Name: "kube-proxy", From: struct {
					Name string `json:"name"`
				}{Name: "quay.io/other:v1"}},
			}

			data, err := json.Marshal(refs)
			Expect(err).NotTo(HaveOccurred())

			result, err := findComponentInData(data, "ovn-kubernetes")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc123"))
		})

		It("should return error when component is not found", func() {
			refs := imageReferences{}
			refs.Spec.Tags = []struct {
				Name string `json:"name"`
				From struct {
					Name string `json:"name"`
				} `json:"from"`
			}{
				{Name: "kube-proxy", From: struct {
					Name string `json:"name"`
				}{Name: "quay.io/other:v1"}},
			}

			data, err := json.Marshal(refs)
			Expect(err).NotTo(HaveOccurred())

			_, err = findComponentInData(data, "ovn-kubernetes")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not found"))
		})

		It("should return error when component has empty image reference", func() {
			refs := imageReferences{}
			refs.Spec.Tags = []struct {
				Name string `json:"name"`
				From struct {
					Name string `json:"name"`
				} `json:"from"`
			}{
				{Name: "ovn-kubernetes", From: struct {
					Name string `json:"name"`
				}{Name: ""}},
			}

			data, err := json.Marshal(refs)
			Expect(err).NotTo(HaveOccurred())

			_, err = findComponentInData(data, "ovn-kubernetes")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("empty image reference"))
		})

		It("should return error for invalid JSON", func() {
			_, err := findComponentInData([]byte("not json"), "ovn-kubernetes")
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("dockerConfigCredentialStore", func() {
		It("should return credentials for matching registry", func() {
			factory, err := newDockerConfigCredentialStoreFactory([]byte(`{"auths":{"quay.io":{"username":"user","password":"pass"}}}`))
			Expect(err).NotTo(HaveOccurred())

			store := factory.CredentialStoreFor("quay.io/some/image")
			u := &url.URL{Host: "quay.io"}
			user, pass := store.Basic(u)
			Expect(user).To(Equal("user"))
			Expect(pass).To(Equal("pass"))
		})

		It("should return empty credentials for non-matching registry", func() {
			factory, err := newDockerConfigCredentialStoreFactory([]byte(`{"auths":{"quay.io":{"username":"user","password":"pass"}}}`))
			Expect(err).NotTo(HaveOccurred())

			store := factory.CredentialStoreFor("other.registry.io/image")
			u := &url.URL{Host: "other.registry.io"}
			user, pass := store.Basic(u)
			Expect(user).To(BeEmpty())
			Expect(pass).To(BeEmpty())
		})

		It("should decode base64 auth field", func() {
			factory, err := newDockerConfigCredentialStoreFactory([]byte(`{"auths":{"quay.io":{"auth":"dXNlcjpwYXNz"}}}`))
			Expect(err).NotTo(HaveOccurred())

			store := factory.CredentialStoreFor("quay.io/image")
			u := &url.URL{Host: "quay.io"}
			user, pass := store.Basic(u)
			Expect(user).To(Equal("user"))
			Expect(pass).To(Equal("pass"))
		})

		It("should return error for invalid JSON", func() {
			_, err := newDockerConfigCredentialStoreFactory([]byte("not json"))
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("splitImage", func() {
		It("should split image with tag", func() {
			repo, tag, err := splitImage("quay.io/openshift/ovn:v4.19.0")
			Expect(err).NotTo(HaveOccurred())
			Expect(repo).To(Equal("quay.io/openshift/ovn"))
			Expect(tag).To(Equal("v4.19.0"))
		})

		It("should split image with sha256 digest", func() {
			repo, tag, err := splitImage("quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abcdef1234567890")
			Expect(err).NotTo(HaveOccurred())
			Expect(repo).To(Equal("quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256"))
			Expect(tag).To(Equal("abcdef1234567890"))
		})

		It("should default to 'latest' tag when no tag present", func() {
			repo, tag, err := splitImage("quay.io/openshift/ovn")
			Expect(err).NotTo(HaveOccurred())
			Expect(repo).To(Equal("quay.io/openshift/ovn"))
			Expect(tag).To(Equal("latest"))
		})

		It("should return error for empty image", func() {
			_, _, err := splitImage("")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("empty image reference"))
		})
	})
})
