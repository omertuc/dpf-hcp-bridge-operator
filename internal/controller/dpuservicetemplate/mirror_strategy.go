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
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	imagereference "github.com/openshift/library-go/pkg/image/reference"
	"github.com/openshift/library-go/pkg/image/registryclient"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ registryclient.AlternateBlobSourceStrategy = &clusterIDMSStrategy{}

// clusterIDMSStrategy implements AlternateBlobSourceStrategy by reading
// ImageDigestMirrorSet CRs from the cluster API. Mirrors are tried first
// so disconnected environments avoid timeouts on an unreachable primary.
type clusterIDMSStrategy struct {
	client client.Client
}

func (s *clusterIDMSStrategy) FirstRequest(ctx context.Context, locator imagereference.DockerImageReference) ([]imagereference.DockerImageReference, error) {
	var idmsList configv1.ImageDigestMirrorSetList
	if err := s.client.List(ctx, &idmsList); err != nil {
		return nil, err
	}

	if len(idmsList.Items) == 0 {
		return nil, nil
	}

	return alternativeImageSourcesFromIDMS(locator, idmsList.Items)
}

func (s *clusterIDMSStrategy) OnFailure(_ context.Context, _ imagereference.DockerImageReference) ([]imagereference.DockerImageReference, error) {
	return nil, nil
}

// alternativeImageSourcesFromIDMS builds an ordered list of mirror refs for the
// given image, with the original source appended last (unless NeverContactSource).
// Ported from oc/pkg/cli/image/strategy/onerror.go:alternativeImageSourcesIDMS.
func alternativeImageSourcesFromIDMS(imageRef imagereference.DockerImageReference, idmsList []configv1.ImageDigestMirrorSet) ([]imagereference.DockerImageReference, error) {
	var mirrors []imagereference.DockerImageReference
	repo := imageRef.AsRepository().Exact()
	addSource := true

	for _, idms := range idmsList {
		for _, rdm := range idms.Spec.ImageDigestMirrors {
			rdmSourceRef, err := imagereference.Parse(rdm.Source)
			if err != nil {
				return nil, err
			}

			var suffix string
			if imageRef.AsRepository().AsV2() != rdmSourceRef.AsRepository().AsV2() {
				if !isSubrepo(repo, rdm.Source) {
					continue
				}
				suffix = repo[len(rdm.Source):]
			}

			if rdm.MirrorSourcePolicy == configv1.NeverContactSource {
				addSource = false
			}

			for _, m := range rdm.Mirrors {
				mRef, err := imagereference.Parse(string(m) + suffix)
				if err != nil {
					return nil, err
				}
				mirrors = append(mirrors, mRef)
			}
		}
	}

	if len(mirrors) == 0 {
		return nil, nil
	}

	result := dedup(mirrors)
	if addSource {
		result = append(result, imageRef.AsRepository().AsV2())
	}
	return result, nil
}

func isSubrepo(repo, ancestor string) bool {
	if repo == ancestor {
		return true
	}
	return len(repo) > len(ancestor) &&
		strings.HasPrefix(repo, ancestor) &&
		repo[len(ancestor)] == '/'
}

func dedup(refs []imagereference.DockerImageReference) []imagereference.DockerImageReference {
	seen := make(map[imagereference.DockerImageReference]struct{}, len(refs))
	out := make([]imagereference.DockerImageReference, 0, len(refs))
	for _, r := range refs {
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}
