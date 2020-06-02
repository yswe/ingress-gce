/*
Copyright 2020 The Kubernetes Authors.
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

package translator

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud/meta"
	api_v1 "k8s.io/api/core/v1"
	"k8s.io/api/networking/v1beta1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"k8s.io/ingress-gce/pkg/composite"
	"k8s.io/ingress-gce/pkg/utils"
	"k8s.io/ingress-gce/pkg/utils/namer"
)

// Env contains all k8s-style configuration needed to perform the translation.
type Env struct {
	// Ing is the Ingress we are translating.
	Ing *v1beta1.Ingress
	// SecretsMap contains a mapping from Secret name to the actual resource.
	// It is assumed that the map contains resources from a single namespace.
	// This is the same namespace as the Ingress namespace.
	SecretsMap map[string]*api_v1.Secret
}

// NewEnv returns an Env for the given Ingress.
func NewEnv(ing *v1beta1.Ingress, client kubernetes.Interface) (*Env, error) {
	ret := &Env{Ing: ing, SecretsMap: make(map[string]*api_v1.Secret)}
	secrets, err := client.CoreV1().Secrets(ing.Namespace).List(context.TODO(), meta_v1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, s := range secrets.Items {
		s := s
		ret.SecretsMap[s.Name] = &s
	}
	return ret, nil
}

// Translator implements the mapping between an MCI and its corresponding GCE resources.
type Translator struct{}

// NewTranslator returns a new Translator.
func NewTranslator() *Translator {
	return &Translator{}
}

// Secrets returns the Secrets from the environment which are specified in the Ingress.
func Secrets(env *Env) ([]*api_v1.Secret, error) {
	var ret []*api_v1.Secret
	spec := env.Ing.Spec
	for _, tlsSpec := range spec.TLS {
		secret, ok := env.SecretsMap[tlsSpec.SecretName]
		if !ok {
			return nil, fmt.Errorf("secret %q does not exist", tlsSpec.SecretName)
		}
		// Fail-fast if the user's secret does not have the proper fields specified.
		if secret.Data[api_v1.TLSCertKey] == nil {
			return nil, fmt.Errorf("secret %q does not specify cert as string data", tlsSpec.SecretName)
		}
		if secret.Data[api_v1.TLSPrivateKeyKey] == nil {
			return nil, fmt.Errorf("secret %q does not specify private key as string data", tlsSpec.SecretName)
		}
		ret = append(ret, secret)
	}

	return ret, nil
}

// The gce api uses the name of a path rule to match a host rule.
const hostRulePrefix = "host"

// ToCompositeURLMap translates the given hostname: endpoint->port mapping into a gce url map.
//
// HostRule: Conceptually contains all PathRules for a given host.
// PathMatcher: Associates a path rule with a host rule. Mostly an optimization.
// PathRule: Maps a single path regex to a backend.
//
// The GCE url map allows multiple hosts to share url->backend mappings without duplication, eg:
//   Host: foo(PathMatcher1), bar(PathMatcher1,2)
//   PathMatcher1:
//     /a -> b1
//     /b -> b2
//   PathMatcher2:
//     /c -> b1
// This leads to a lot of complexity in the common case, where all we want is a mapping of
// host->{/path: backend}.
//
// Consider some alternatives:
// 1. Using a single backend per PathMatcher:
//   Host: foo(PathMatcher1,3) bar(PathMatcher1,2,3)
//   PathMatcher1:
//     /a -> b1
//   PathMatcher2:
//     /c -> b1
//   PathMatcher3:
//     /b -> b2
// 2. Using a single host per PathMatcher:
//   Host: foo(PathMatcher1)
//   PathMatcher1:
//     /a -> b1
//     /b -> b2
//   Host: bar(PathMatcher2)
//   PathMatcher2:
//     /a -> b1
//     /b -> b2
//     /c -> b1
// In the context of kubernetes services, 2 makes more sense, because we
// rarely want to lookup backends (service:nodeport). When a service is
// deleted, we need to find all host PathMatchers that have the backend
// and remove the mapping. When a new path is added to a host (happens
// more frequently than service deletion) we just need to lookup the 1
// pathmatcher of the host.
func ToCompositeURLMap(g *utils.GCEURLMap, namer namer.IngressFrontendNamer, key *meta.Key) *composite.UrlMap {
	defaultBackendName := g.DefaultBackend.BackendName()
	key.Name = defaultBackendName
	resourceID := cloud.ResourceID{ProjectID: "", Resource: "backendServices", Key: key}
	m := &composite.UrlMap{
		Name:           namer.UrlMap(),
		DefaultService: resourceID.ResourcePath(),
	}

	for _, hostRule := range g.HostRules {
		// Create a host rule
		// Create a path matcher
		// Add all given endpoint:backends to pathRules in path matcher
		pmName := getNameForPathMatcher(hostRule.Hostname)
		m.HostRules = append(m.HostRules, &composite.HostRule{
			Hosts:       []string{hostRule.Hostname},
			PathMatcher: pmName,
		})

		pathMatcher := &composite.PathMatcher{
			Name:           pmName,
			DefaultService: m.DefaultService,
			PathRules:      []*composite.PathRule{},
		}

		// GCE ensures that matched rule with longest prefix wins.
		for _, rule := range hostRule.Paths {
			beName := rule.Backend.BackendName()
			key.Name = beName
			resourceID := cloud.ResourceID{ProjectID: "", Resource: "backendServices", Key: key}
			beLink := resourceID.ResourcePath()
			pathMatcher.PathRules = append(pathMatcher.PathRules, &composite.PathRule{
				Paths:   []string{rule.Path},
				Service: beLink,
			})
		}
		m.PathMatchers = append(m.PathMatchers, pathMatcher)
	}
	return m
}

// getNameForPathMatcher returns a name for a pathMatcher based on the given host rule.
// The host rule can be a regex, the path matcher name used to associate the 2 cannot.
func getNameForPathMatcher(hostRule string) string {
	hasher := md5.New()
	hasher.Write([]byte(hostRule))
	return fmt.Sprintf("%v%v", hostRulePrefix, hex.EncodeToString(hasher.Sum(nil)))
}