package runner

import "testing"

func TestCIRejectsFailure(t *testing.T) {
	t.Fatal("CI acceptance: intentional Go test failure")
}
