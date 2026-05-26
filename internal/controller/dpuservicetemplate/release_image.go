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
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/distribution/distribution/v3"
	"github.com/distribution/distribution/v3/manifest/schema2"
	"github.com/distribution/distribution/v3/registry/client/auth"
	"github.com/opencontainers/go-digest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	imagereference "github.com/openshift/library-go/pkg/image/reference"
	"github.com/openshift/library-go/pkg/image/registryclient"
)

const (
	imageReferencesPath = "release-manifests/image-references"
	ovnKubernetesName   = "ovn-kubernetes"
)

// ReleaseImageReader extracts component images from an OCP release payload.
type ReleaseImageReader interface {
	GetComponentImage(ctx context.Context, releaseImageRef string, componentName string) (string, error)
}

// RemoteReleaseImageReader pulls an OCP release image from a registry (with
// mirror support via IDMS) and extracts component image references from its
// image-references manifest.
type RemoteReleaseImageReader struct {
	client client.Client
}

// NewRemoteReleaseImageReader creates a new reader that resolves images using
// the cluster pull secret for auth and ImageDigestMirrorSet CRs for mirrors.
func NewRemoteReleaseImageReader(c client.Client) *RemoteReleaseImageReader {
	return &RemoteReleaseImageReader{client: c}
}

func (r *RemoteReleaseImageReader) GetComponentImage(ctx context.Context, releaseImageRef string, componentName string) (string, error) {
	regCtx, err := r.buildRegistryContext(ctx)
	if err != nil {
		return "", fmt.Errorf("building registry context: %w", err)
	}

	ref, err := imagereference.Parse(releaseImageRef)
	if err != nil {
		return "", fmt.Errorf("parsing release image ref %q: %w", releaseImageRef, err)
	}

	repo, err := regCtx.RepositoryForRef(ctx, ref, false)
	if err != nil {
		return "", fmt.Errorf("connecting to registry for %q: %w", releaseImageRef, err)
	}

	data, err := extractFileFromLayers(ctx, repo, ref, imageReferencesPath)
	if err != nil {
		return "", fmt.Errorf("extracting %s from release image %q: %w", imageReferencesPath, releaseImageRef, err)
	}

	return findComponentInData(data, componentName)
}

func (r *RemoteReleaseImageReader) buildRegistryContext(ctx context.Context) (*registryclient.Context, error) {
	secret := &corev1.Secret{}
	if err := r.client.Get(ctx, types.NamespacedName{
		Name:      clusterPullSecretName,
		Namespace: clusterPullSecretNamespace,
	}, secret); err != nil {
		return nil, fmt.Errorf("getting secret %s/%s: %w", clusterPullSecretNamespace, clusterPullSecretName, err)
	}

	dockerConfigJSON, ok := secret.Data[clusterPullSecretKey]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s missing key %q", clusterPullSecretNamespace, clusterPullSecretName, clusterPullSecretKey)
	}

	credFactory, err := newDockerConfigCredentialStoreFactory(dockerConfigJSON)
	if err != nil {
		return nil, fmt.Errorf("parsing pull secret: %w", err)
	}

	insecureRT := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	regCtx := registryclient.NewContext(http.DefaultTransport, insecureRT).
		WithCredentialsFactory(credFactory).
		WithAlternateBlobSourceStrategy(&clusterIDMSStrategy{client: r.client})

	return regCtx, nil
}

// extractFileFromLayers resolves the image manifest and walks the layers
// (topmost first) looking for the given file path in the tar archives.
func extractFileFromLayers(ctx context.Context, repo distribution.Repository, ref imagereference.DockerImageReference, filePath string) ([]byte, error) {
	manifests, err := repo.Manifests(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting manifest service: %w", err)
	}

	var dgst digest.Digest
	if ref.ID != "" {
		dgst = digest.Digest(ref.ID)
	} else {
		desc, err := repo.Tags(ctx).Get(ctx, ref.Tag)
		if err != nil {
			return nil, fmt.Errorf("resolving tag %q: %w", ref.Tag, err)
		}
		dgst = desc.Digest
	}

	manifest, err := manifests.Get(ctx, dgst)
	if err != nil {
		return nil, fmt.Errorf("getting manifest: %w", err)
	}

	layers := layersFromManifest(manifest)

	blobs := repo.Blobs(ctx)
	for i := len(layers) - 1; i >= 0; i-- {
		data, err := findFileInLayer(ctx, blobs, layers[i].Digest, filePath)
		if err != nil {
			continue
		}
		if data != nil {
			return data, nil
		}
	}

	return nil, fmt.Errorf("file %s not found in any image layer", filePath)
}

func layersFromManifest(manifest distribution.Manifest) []distribution.Descriptor {
	if m, ok := manifest.(*schema2.DeserializedManifest); ok {
		return m.Layers
	}
	var layers []distribution.Descriptor
	for _, ref := range manifest.References() {
		if strings.Contains(ref.MediaType, "layer") {
			layers = append(layers, ref)
		}
	}
	return layers
}

func findFileInLayer(ctx context.Context, blobs distribution.BlobStore, dgst digest.Digest, filePath string) ([]byte, error) {
	r, err := blobs.Open(ctx, dgst)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == filePath {
			return io.ReadAll(tr)
		}
	}
}

// imageReferences is a minimal representation of the OpenShift ImageStream
// stored at release-manifests/image-references in a release payload image.
type imageReferences struct {
	Spec struct {
		Tags []struct {
			Name string `json:"name"`
			From struct {
				Name string `json:"name"`
			} `json:"from"`
		} `json:"tags"`
	} `json:"spec"`
}

func findComponentInData(data []byte, componentName string) (string, error) {
	var refs imageReferences
	if err := json.Unmarshal(data, &refs); err != nil {
		return "", fmt.Errorf("parsing %s: %w", imageReferencesPath, err)
	}

	for _, tag := range refs.Spec.Tags {
		if tag.Name == componentName {
			if tag.From.Name == "" {
				return "", fmt.Errorf("component %q has empty image reference", componentName)
			}
			return tag.From.Name, nil
		}
	}

	return "", fmt.Errorf("component %q not found in %s", componentName, imageReferencesPath)
}

// splitImage splits a container image reference into repository and tag/digest.
func splitImage(image string) (repo, tag string, err error) {
	if image == "" {
		return "", "", fmt.Errorf("empty image reference")
	}

	if idx := strings.LastIndex(image, "@sha256:"); idx > 0 {
		return image[:idx] + "@sha256", image[idx+len("@sha256:"):], nil
	}

	lastColon := strings.LastIndex(image, ":")
	if lastColon > 0 && !strings.Contains(image[lastColon:], "/") {
		return image[:lastColon], image[lastColon+1:], nil
	}

	return image, "latest", nil
}

// dockerConfigCredentialStoreFactory implements registryclient.CredentialStoreFactory
// using a parsed .dockerconfigjson.
type dockerConfigCredentialStoreFactory struct {
	auths map[string]dockerConfigAuth
}

type dockerConfigAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

func newDockerConfigCredentialStoreFactory(dockerConfigJSON []byte) (*dockerConfigCredentialStoreFactory, error) {
	var config struct {
		Auths map[string]dockerConfigAuth `json:"auths"`
	}
	if err := json.Unmarshal(dockerConfigJSON, &config); err != nil {
		return nil, err
	}
	return &dockerConfigCredentialStoreFactory{auths: config.Auths}, nil
}

func (f *dockerConfigCredentialStoreFactory) CredentialStoreFor(image string) auth.CredentialStore {
	return &dockerConfigCredentialStore{auths: f.auths}
}

// dockerConfigCredentialStore implements auth.CredentialStore.
type dockerConfigCredentialStore struct {
	auths map[string]dockerConfigAuth
}

func (s *dockerConfigCredentialStore) Basic(u *url.URL) (string, string) {
	a, ok := s.auths[u.Host]
	if !ok {
		return "", ""
	}
	if a.Username != "" {
		return a.Username, a.Password
	}
	if a.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(a.Auth)
		if err != nil {
			return "", ""
		}
		parts := strings.SplitN(string(decoded), ":", 2)
		if len(parts) == 2 {
			return parts[0], parts[1]
		}
	}
	return "", ""
}

func (s *dockerConfigCredentialStore) RefreshToken(_ *url.URL, _ string) string {
	return ""
}

func (s *dockerConfigCredentialStore) SetRefreshToken(_ *url.URL, _, _ string) {}

