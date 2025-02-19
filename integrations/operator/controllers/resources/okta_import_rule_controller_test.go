/*
Copyright 2022 Gravitational, Inc.

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

package resources_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/gravitational/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gravitational/teleport/api/types"
	resourcesv1 "github.com/gravitational/teleport/integrations/operator/apis/resources/v1"
	"github.com/gravitational/teleport/integrations/operator/controllers/resources/testlib"
	"github.com/gravitational/teleport/lib/utils"
)

var oktaImportRuleSpec = types.OktaImportRuleSpecV1{
	Priority: 100,
	Mappings: []*types.OktaImportRuleMappingV1{
		{
			Match: []*types.OktaImportRuleMatchV1{
				{
					AppIDs: []string{"1", "2", "3"},
				},
			},
			AddLabels: map[string]string{
				"label1": "value1",
			},
		},
		{
			Match: []*types.OktaImportRuleMatchV1{
				{
					GroupIDs: []string{"1", "2", "3"},
				},
			},
			AddLabels: map[string]string{
				"label2": "value2",
			},
		},
	},
}

type oktaImportRuleTestingPrimitives struct {
	setup *testSetup
}

func (g *oktaImportRuleTestingPrimitives) Init(setup *testSetup) {
	g.setup = setup
}

func (g *oktaImportRuleTestingPrimitives) SetupTeleportFixtures(ctx context.Context) error {
	return nil
}

func (g *oktaImportRuleTestingPrimitives) CreateTeleportResource(ctx context.Context, name string) error {
	importRule, err := types.NewOktaImportRule(types.Metadata{
		Name: name,
	}, oktaImportRuleSpec)
	if err != nil {
		return trace.Wrap(err)
	}
	importRule.SetOrigin(types.OriginKubernetes)
	_, err = g.setup.TeleportClient.OktaClient().CreateOktaImportRule(ctx, importRule)
	return trace.Wrap(err)
}

func (g *oktaImportRuleTestingPrimitives) GetTeleportResource(ctx context.Context, name string) (types.OktaImportRule, error) {
	return g.setup.TeleportClient.OktaClient().GetOktaImportRule(ctx, name)
}

func (g *oktaImportRuleTestingPrimitives) DeleteTeleportResource(ctx context.Context, name string) error {
	return trace.Wrap(g.setup.TeleportClient.OktaClient().DeleteOktaImportRule(ctx, name))
}

func (g *oktaImportRuleTestingPrimitives) CreateKubernetesResource(ctx context.Context, name string) error {
	spec := resourcesv1.TeleportOktaImportRuleSpec{
		Priority: oktaImportRuleSpec.Priority,
		Mappings: make([]resourcesv1.TeleportOktaImportRuleMapping, len(oktaImportRuleSpec.Mappings)),
	}

	for i, mapping := range oktaImportRuleSpec.Mappings {
		matches := make([]resourcesv1.TeleportOktaImportRuleMatch, len(mapping.Match))
		for j, match := range mapping.Match {
			matches[j] = resourcesv1.TeleportOktaImportRuleMatch{
				AppIDs:           match.AppIDs,
				GroupIDs:         match.GroupIDs,
				AppNameRegexes:   match.AppNameRegexes,
				GroupNameRegexes: match.GroupNameRegexes,
			}
		}
		spec.Mappings[i] = resourcesv1.TeleportOktaImportRuleMapping{
			Match:     matches,
			AddLabels: utils.CopyStringsMap(mapping.AddLabels),
		}
	}

	importRule := &resourcesv1.TeleportOktaImportRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: g.setup.Namespace.Name,
		},
		Spec: spec,
	}
	return trace.Wrap(g.setup.K8sClient.Create(ctx, importRule))
}

func (g *oktaImportRuleTestingPrimitives) DeleteKubernetesResource(ctx context.Context, name string) error {
	oidc := &resourcesv1.TeleportOktaImportRule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: g.setup.Namespace.Name,
		},
	}
	return trace.Wrap(g.setup.K8sClient.Delete(ctx, oidc))
}

func (g *oktaImportRuleTestingPrimitives) GetKubernetesResource(ctx context.Context, name string) (*resourcesv1.TeleportOktaImportRule, error) {
	importRule := &resourcesv1.TeleportOktaImportRule{}
	obj := kclient.ObjectKey{
		Name:      name,
		Namespace: g.setup.Namespace.Name,
	}
	err := g.setup.K8sClient.Get(ctx, obj, importRule)
	return importRule, trace.Wrap(err)
}

func (g *oktaImportRuleTestingPrimitives) ModifyKubernetesResource(ctx context.Context, name string) error {
	importRule, err := g.GetKubernetesResource(ctx, name)
	if err != nil {
		return trace.Wrap(err)
	}
	importRule.Spec.Priority = 50
	return g.setup.K8sClient.Update(ctx, importRule)
}

func (g *oktaImportRuleTestingPrimitives) CompareTeleportAndKubernetesResource(tResource types.OktaImportRule, kubeResource *resourcesv1.TeleportOktaImportRule) (bool, string) {
	teleportMap, _ := teleportResourceToMap(tResource)
	kubernetesMap, _ := teleportResourceToMap(kubeResource.ToTeleport())

	equal := cmp.Equal(teleportMap["spec"], kubernetesMap["spec"])
	if !equal {
		return equal, cmp.Diff(teleportMap["spec"], kubernetesMap["spec"])
	}

	return equal, ""
}

func TestOktaImportRuleCreation(t *testing.T) {
	test := &oktaImportRuleTestingPrimitives{}
	testlib.ResourceCreationTest[types.OktaImportRule, *resourcesv1.TeleportOktaImportRule](t, test)
}

func TestOktaImportRuleDeletionDrift(t *testing.T) {
	test := &oktaImportRuleTestingPrimitives{}
	testlib.ResourceDeletionDriftTest[types.OktaImportRule, *resourcesv1.TeleportOktaImportRule](t, test)
}

func TestOktaImportRuleUpdate(t *testing.T) {
	test := &oktaImportRuleTestingPrimitives{}
	testlib.ResourceUpdateTest[types.OktaImportRule, *resourcesv1.TeleportOktaImportRule](t, test)
}
