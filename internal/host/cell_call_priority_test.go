package host

import (
	"context"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/abi"
)

func TestStepYieldsIdleWorkToWaitingProviderCall(t *testing.T) {
	var cell Cell
	cell.callWaiters.Store(1)

	output, err := cell.Step(context.Background(), abi.StepEnvelope{})
	if err != nil {
		t.Fatalf("idle step with waiting call: %v", err)
	}
	if output != 0 {
		t.Fatalf("idle step output = %d, want 0", output)
	}
}

func TestStepDoesNotDropPayloadForWaitingProviderCall(t *testing.T) {
	var cell Cell
	cell.callWaiters.Store(1)

	_, err := cell.Step(context.Background(), abi.StepEnvelope{Payload: []byte{1}})
	if err == nil || err.Error() != "cell is closed" {
		t.Fatalf("payload step error = %v, want cell is closed", err)
	}
}
