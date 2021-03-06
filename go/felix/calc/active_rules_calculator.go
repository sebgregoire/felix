// Copyright (c) 2016 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package calc

import (
	log "github.com/Sirupsen/logrus"
	"github.com/projectcalico/felix/go/felix/dispatcher"
	"github.com/projectcalico/felix/go/felix/labelindex"
	"github.com/projectcalico/felix/go/felix/multidict"
	"github.com/projectcalico/felix/go/felix/tagindex"
	"github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	"github.com/projectcalico/libcalico-go/lib/selector"
	"reflect"
)

type ruleScanner interface {
	OnPolicyActive(model.PolicyKey, *model.Policy)
	OnPolicyInactive(model.PolicyKey)
	OnProfileActive(model.ProfileRulesKey, *model.ProfileRules)
	OnProfileInactive(model.ProfileRulesKey)
}

type FelixSender interface {
	SendUpdateToFelix(update model.KVPair)
}

type PolicyMatchListener interface {
	OnPolicyMatch(policyKey model.PolicyKey, endpointKey interface{})
	OnPolicyMatchStopped(policyKey model.PolicyKey, endpointKey interface{})
}

type ActiveRulesCalculator struct {
	// Caches of all known policies/profiles.
	allPolicies     map[model.PolicyKey]*model.Policy
	allProfileRules map[string]*model.ProfileRules

	// Policy/profile ID to matching endpoint sets.
	policyIDToEndpointKeys  multidict.IfaceToIface
	profileIDToEndpointKeys multidict.IfaceToIface

	// Label index, matching policy selectors against local endpoints.
	labelIndex *labelindex.InheritIndex

	// Cache of profile IDs by local endpoint.
	endpointKeyToProfileIDs *tagindex.EndpointKeyToProfileIDMap

	// Callback objects.
	RuleScanner         ruleScanner
	PolicyMatchListener PolicyMatchListener
}

func NewActiveRulesCalculator() *ActiveRulesCalculator {
	arc := &ActiveRulesCalculator{
		// Caches of all known policies/profiles.
		allPolicies:     make(map[model.PolicyKey]*model.Policy),
		allProfileRules: make(map[string]*model.ProfileRules),

		// Policy/profile ID to matching endpoint sets.
		policyIDToEndpointKeys:  multidict.NewIfaceToIface(),
		profileIDToEndpointKeys: multidict.NewIfaceToIface(),

		// Cache of profile IDs by local endpoint.
		endpointKeyToProfileIDs: tagindex.NewEndpointKeyToProfileIDMap(),
	}
	arc.labelIndex = labelindex.NewInheritIndex(arc.onMatchStarted, arc.onMatchStopped)
	return arc
}

func (arc *ActiveRulesCalculator) RegisterWith(localEndpointDispatcher, allUpdDispatcher *dispatcher.Dispatcher) {
	// It needs the filtered endpoints...
	localEndpointDispatcher.Register(model.WorkloadEndpointKey{}, arc.OnUpdate)
	localEndpointDispatcher.Register(model.HostEndpointKey{}, arc.OnUpdate)
	// ...as well as all the policies and profiles.
	allUpdDispatcher.Register(model.PolicyKey{}, arc.OnUpdate)
	allUpdDispatcher.Register(model.ProfileRulesKey{}, arc.OnUpdate)
	allUpdDispatcher.Register(model.ProfileLabelsKey{}, arc.OnUpdate)
	allUpdDispatcher.Register(model.ProfileTagsKey{}, arc.OnUpdate)
}

func (arc *ActiveRulesCalculator) OnUpdate(update api.Update) (filterOut bool) {
	switch key := update.Key.(type) {
	case model.WorkloadEndpointKey:
		if update.Value != nil {
			log.Debugf("Updating ARC with endpoint %v", key)
			endpoint := update.Value.(*model.WorkloadEndpoint)
			profileIDs := endpoint.ProfileIDs
			arc.updateEndpointProfileIDs(key, profileIDs)
		} else {
			log.Debugf("Deleting endpoint %v from ARC", key)
			arc.updateEndpointProfileIDs(key, []string{})
		}
		arc.labelIndex.OnUpdate(update)
	case model.HostEndpointKey:
		if update.Value != nil {
			// Figure out what's changed and update the cache.
			log.Debugf("Updating ARC for host endpoint %v", key)
			endpoint := update.Value.(*model.HostEndpoint)
			profileIDs := endpoint.ProfileIDs
			arc.updateEndpointProfileIDs(key, profileIDs)
		} else {
			log.Debugf("Deleting host endpoint %v from ARC", key)
			arc.updateEndpointProfileIDs(key, []string{})
		}
		arc.labelIndex.OnUpdate(update)
	case model.ProfileLabelsKey:
		arc.labelIndex.OnUpdate(update)
	case model.ProfileTagsKey:
		arc.labelIndex.OnUpdate(update)
	case model.ProfileRulesKey:
		if update.Value != nil {
			rules := update.Value.(*model.ProfileRules)
			arc.allProfileRules[key.Name] = rules
			if arc.profileIDToEndpointKeys.ContainsKey(key.Name) {
				log.Debugf("Profile rules updated while active: %v", key.Name)
				arc.sendProfileUpdate(key.Name)
			} else {
				log.Debugf("Profile rules updated while inactive: %v", key.Name)
			}
		} else {
			delete(arc.allProfileRules, key.Name)
			if arc.profileIDToEndpointKeys.ContainsKey(key.Name) {
				log.Debug("Profile rules deleted while active, telling listener/felix")
				arc.sendProfileUpdate(key.Name)
			} else {
				log.Debugf("Profile rules deleted while inactive: %v", key.Name)
			}
		}
	case model.PolicyKey:
		if update.Value != nil {
			log.Debugf("Updating ARC for policy %v", key)
			policy := update.Value.(*model.Policy)
			arc.allPolicies[key] = policy
			// Update the index, which will call us back if the selector no
			// longer matches.
			sel, err := selector.Parse(policy.Selector)
			if err != nil {
				log.Fatal(err)
			}
			arc.labelIndex.UpdateSelector(key, sel)

			if arc.policyIDToEndpointKeys.ContainsKey(key) {
				// If we get here, the selector still matches something,
				// update the rules.
				// TODO: squash duplicate update if labelIndex.UpdateSelector already made this active
				log.Debug("Policy updated while active, telling listener")
				arc.sendPolicyUpdate(key)
			}
		} else {
			log.Debugf("Removing policy %v from ARC", key)
			delete(arc.allPolicies, key)
			arc.labelIndex.DeleteSelector(key)
			// No need to call updatePolicy() because we'll have got a matchStopped
			// callback.
		}
	default:
		log.Infof("Ignoring unexpected update: %v %#v",
			reflect.TypeOf(update.Key), update)
	}
	return
}

func (arc *ActiveRulesCalculator) updateEndpointProfileIDs(key model.Key, profileIDs []string) {
	// Figure out which profiles have been added/removed.
	log.Debugf("Endpoint %#v now has profile IDs: %v", key, profileIDs)
	removedIDs, addedIDs := arc.endpointKeyToProfileIDs.Update(key, profileIDs)

	// Update the index of required profile IDs for added profiles,
	// triggering events for profiles that just became active.
	for id := range addedIDs {
		wasActive := arc.profileIDToEndpointKeys.ContainsKey(id)
		arc.profileIDToEndpointKeys.Put(id, key)
		if !wasActive {
			// This profile is now active.
			arc.sendProfileUpdate(id)
		}
	}

	// Update the index for no-longer required profile IDs, triggering
	// events for profiles that just became inactive.
	for id := range removedIDs {
		arc.profileIDToEndpointKeys.Discard(id, key)
		if !arc.profileIDToEndpointKeys.ContainsKey(id) {
			// No endpoint refers to this ID any more.  Clean it
			// up.
			arc.sendProfileUpdate(id)
		}
	}
}

func (arc *ActiveRulesCalculator) onMatchStarted(selID, labelId interface{}) {
	polKey := selID.(model.PolicyKey)
	policyWasActive := arc.policyIDToEndpointKeys.ContainsKey(polKey)
	arc.policyIDToEndpointKeys.Put(selID, labelId)
	if !policyWasActive {
		// Policy wasn't active before, tell the listener.  The policy
		// must be in allPolicies because we can only match on a policy
		// that we've seen.
		log.Debugf("Policy %v now matches a local endpoint", polKey)
		arc.sendPolicyUpdate(polKey)
	}
	arc.PolicyMatchListener.OnPolicyMatch(polKey, labelId)
}

func (arc *ActiveRulesCalculator) onMatchStopped(selID, labelId interface{}) {
	polKey := selID.(model.PolicyKey)
	arc.policyIDToEndpointKeys.Discard(selID, labelId)
	if !arc.policyIDToEndpointKeys.ContainsKey(selID) {
		// Policy no longer active.
		polKey := selID.(model.PolicyKey)
		log.Debugf("Policy %v no longer matches a local endpoint", polKey)
		arc.sendPolicyUpdate(polKey)
	}
	arc.PolicyMatchListener.OnPolicyMatchStopped(polKey, labelId)
}

func (arc *ActiveRulesCalculator) sendProfileUpdate(profileID string) {
	active := arc.profileIDToEndpointKeys.ContainsKey(profileID)
	rules, known := arc.allProfileRules[profileID]
	log.Debugf("Sending profile update for profile %v (known: %v, active: %v)",
		profileID, known, active)
	key := model.ProfileRulesKey{ProfileKey: model.ProfileKey{Name: profileID}}

	if known && active {
		arc.RuleScanner.OnProfileActive(key, rules)
	} else {
		arc.RuleScanner.OnProfileInactive(key)
	}
}

func (arc *ActiveRulesCalculator) sendPolicyUpdate(policyKey model.PolicyKey) {
	policy, known := arc.allPolicies[policyKey]
	active := arc.policyIDToEndpointKeys.ContainsKey(policyKey)
	log.Debugf("Sending policy update for policy %v (known: %v, active: %v)",
		policyKey, known, active)
	if known && active {
		arc.RuleScanner.OnPolicyActive(policyKey, policy)
	} else {
		arc.RuleScanner.OnPolicyInactive(policyKey)
	}
}
