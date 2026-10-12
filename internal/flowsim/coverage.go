// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"slices"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// Coverage is what a set of reports left unreached in a flow.
type Coverage struct {
	// Phases is the number of phases the flow has.
	Phases int
	// UnreachedPhases are phases no scenario entered, sorted.
	UnreachedPhases []string
	// Transitions is the number of phase-local transitions the flow declares.
	Transitions int
	// UnwalkedTransitions are phase-local transitions whose from -> target
	// move no scenario made, sorted, as "from -> target". Moves made by the
	// flow-wide rules and the engine's guards are not attributed to any
	// declared transition; their targets count as phases reached.
	UnwalkedTransitions []string
}

// Measure computes the coverage of reports over a flow. A move is credited to
// a declared transition when it has the same source and target, which is
// exact for a phase whose transitions have distinct targets and a close
// approximation otherwise.
func Measure(spec *flow.Spec, reports []Report) Coverage {
	reached := map[string]bool{}
	walked := map[Move]bool{}
	for _, report := range reports {
		for _, id := range report.Path {
			reached[id] = true
		}
		for _, move := range report.Moves {
			walked[move] = true
		}
	}

	c := Coverage{Phases: len(spec.Nodes)}
	for id, node := range spec.Nodes {
		if !reached[id] {
			c.UnreachedPhases = append(c.UnreachedPhases, id)
		}
		for _, rule := range node.Transitions {
			c.Transitions++
			if rule.Target == id || walked[Move{From: id, To: rule.Target}] {
				continue
			}
			name := id + " -> " + rule.Target
			if !slices.Contains(c.UnwalkedTransitions, name) {
				c.UnwalkedTransitions = append(c.UnwalkedTransitions, name)
			}
		}
	}
	slices.Sort(c.UnreachedPhases)
	slices.Sort(c.UnwalkedTransitions)
	return c
}
