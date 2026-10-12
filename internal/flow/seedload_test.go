// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"os"
	"path/filepath"
	"testing"
)

// The reference flows are part of the product; every one must load.
func TestSeedFlowsLoad(t *testing.T) {
	files, err := filepath.Glob("../seed/flows/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no seed flows found: %v", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Load(data); err != nil {
			t.Errorf("%s: %v", filepath.Base(file), err)
		}
	}
}
