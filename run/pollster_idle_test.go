package run

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestExtensionPollersDoNotMicrosecondBusyWait(t *testing.T) {
	if extensionPollIdleInterval < 10*time.Millisecond {
		t.Fatalf("extension poll idle interval = %s, want at least 10ms", extensionPollIdleInterval)
	}

	source, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(source), "time.Sleep(extensionPollIdleInterval)"); got != 3 {
		t.Fatalf("paced extension poll loops = %d, want 3", got)
	}
}

func TestFusedEventOnlyCellsDoNotReceiveSyntheticIdleSteps(t *testing.T) {
	source, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, required := range []string{
		`isInbound := rt.declared["transport.http.inbound"]`,
		"if !isInbound",
		"Event-only cells are driven exclusively by declared extension events",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("fused idle pacing contract missing %q", required)
		}
	}
	if strings.Contains(text, "idleMax := 30 * time.Second") {
		t.Fatal("event-only synthetic liveness tick remains enabled")
	}
}
