// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/mockbackend"
)

// StartMockBackend serves the reference mock backend in-process and returns a
// flow.Backend aimed at it, with the function that stops it.
func StartMockBackend() (*flow.Backend, func()) {
	server := httptest.NewServer(mockbackend.New(nil))
	return flow.NewBackend(server.URL), server.Close
}

// Write prints reports and coverage for one flow. verbose adds each step.
// It returns the number of failed scenarios.
func Write(w io.Writer, flowID string, reports []Report, coverage Coverage, verbose bool) int {
	failed := 0
	fmt.Fprintf(w, "flow %s\n", flowID)
	for _, r := range reports {
		status := "PASS"
		if !r.Passed() {
			status = "FAIL"
			failed++
		}
		fmt.Fprintf(w, "  %s  %s\n", status, r.Scenario)
		fmt.Fprintf(w, "        path:   %s\n", strings.Join(r.Path, " > "))
		fmt.Fprintf(w, "        ending: %s\n", r.Ending)
		if verbose {
			for i, line := range r.Log {
				fmt.Fprintf(w, "        %2d. %s\n", i+1, line)
			}
		}
		if !r.Passed() {
			fmt.Fprintf(w, "        failure: %s\n", r.Failure)
		}
	}
	fmt.Fprintf(w, "  coverage: %d/%d phases reached, %d/%d phase transitions walked\n",
		coverage.Phases-len(coverage.UnreachedPhases), coverage.Phases,
		coverage.Transitions-len(coverage.UnwalkedTransitions), coverage.Transitions)
	if len(coverage.UnreachedPhases) > 0 {
		fmt.Fprintf(w, "  unreached phases: %s\n", strings.Join(coverage.UnreachedPhases, ", "))
	}
	if len(coverage.UnwalkedTransitions) > 0 {
		fmt.Fprintf(w, "  unwalked transitions: %s\n", strings.Join(coverage.UnwalkedTransitions, "; "))
	}
	return failed
}
