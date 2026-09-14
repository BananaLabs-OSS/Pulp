package run

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestClosedModuleCallErrorClassification(t *testing.T) {
	for _, message := range []string{
		"pulp_on_call trap: module closed with context deadline exceeded",
		"write name: pulp_alloc trap: module has already been closed",
	} {
		if !closedModuleCallError(errors.New(message)) {
			t.Fatalf("did not classify %q", message)
		}
	}
	if closedModuleCallError(errors.New("pulp_on_call returned 4")) {
		t.Fatal("ordinary application error classified as a closed module")
	}
}

func TestSynchronousRecoveryDoesNotReplayFailedCall(t *testing.T) {
	raw, err := os.ReadFile("sibling.go")
	if err != nil {
		t.Fatal(err)
	}
	section := string(raw)
	start := strings.Index(section, "func callRuntimeProvider")
	end := strings.Index(section[start:], "func closedModuleCallError")
	if start < 0 || end < 0 {
		t.Fatal("synchronous recovery boundary missing")
	}
	section = section[start : start+end]
	if strings.Count(section, ".Call(ctx, provider, args)") != 1 {
		t.Fatal("synchronous recovery must never replay an ambiguous failed call")
	}
}
