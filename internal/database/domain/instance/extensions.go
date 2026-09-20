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

package instance

import "slices"

// ExtensionSet is an immutable set of exact names. Zero represents the empty set.
// It does not impose identifier syntax or claim that a server supports any name.
type ExtensionSet struct {
	names []string
}

func NewExtensionSet(names []string) ExtensionSet {
	copied := slices.Clone(names)
	slices.Sort(copied)
	return ExtensionSet{names: slices.Compact(copied)}
}

// Names returns a sorted, deduplicated copy.
func (s ExtensionSet) Names() []string { return slices.Clone(s.names) }

type ExtensionDecision string

const (
	ExtensionsAccepted         ExtensionDecision = "Accepted"
	ExtensionsUnsupported      ExtensionDecision = "ExtensionsUnsupported"
	ExtensionSupportUnobserved ExtensionDecision = "ExtensionSupportUnobserved"
)

// ExtensionCheck reports support only, not readiness or permission to install.
// Unsupported is a detached, sorted list and is populated only for known support.
type ExtensionCheck struct {
	Decision    ExtensionDecision
	Unsupported []string
}

// ExtensionSupport is the extension-list component of an Instance observation.
// Zero means unobserved, not an observed empty list. Target/revision binding and
// invalidation belong to the containing Instance observation, not this set value.
type ExtensionSupport struct {
	observed  bool
	available ExtensionSet
}

// ObserveExtensionSupport records a successfully read list, including an empty one.
// A failed query must not call this constructor with an empty list: the application
// must propagate the dependency failure and leave support unobserved.
func ObserveExtensionSupport(available []string) ExtensionSupport {
	return ExtensionSupport{observed: true, available: NewExtensionSet(available)}
}

// Check performs no IO and cannot install or remove extensions.
func (s ExtensionSupport) Check(requested ExtensionSet) ExtensionCheck {
	if len(requested.names) == 0 {
		return ExtensionCheck{Decision: ExtensionsAccepted}
	}
	if !s.observed {
		return ExtensionCheck{Decision: ExtensionSupportUnobserved}
	}
	var unsupported []string
	for _, name := range requested.names {
		if _, found := slices.BinarySearch(s.available.names, name); !found {
			unsupported = append(unsupported, name)
		}
	}
	if len(unsupported) != 0 {
		return ExtensionCheck{Decision: ExtensionsUnsupported, Unsupported: unsupported}
	}
	return ExtensionCheck{Decision: ExtensionsAccepted}
}
