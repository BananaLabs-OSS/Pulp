package abi

import "github.com/vmihailenco/msgpack/v5"

// Event kinds delivered to the cell as the step envelope payload.
const (
	EventHTTPRequest = "http.request"
	EventWSOpen      = "ws.open"
	EventWSFrame     = "ws.frame"
	EventWSClose     = "ws.close"
)

// StepEvent is the outer MessagePack envelope wrapping any event the host
// delivers to pulp_step. Kind selects which concrete struct Payload
// decodes to. An empty envelope payload (envelope.Payload == nil) means
// "no event this step" — the cell should treat it as a tick and return.
type StepEvent struct {
	Kind    string             `msgpack:"kind"`
	Payload msgpack.RawMessage `msgpack:"payload"`
}

// EncodeStepEvent wraps a pre-encoded payload with the kind discriminator
// and marshals the result for delivery via the step envelope.
func EncodeStepEvent(kind string, payload []byte) ([]byte, error) {
	return msgpack.Marshal(StepEvent{Kind: kind, Payload: payload})
}

// DecodeStepEvent unmarshals the step envelope payload into a StepEvent.
// Callers inspect Kind to decide how to decode Payload.
func DecodeStepEvent(data []byte) (StepEvent, error) {
	var ev StepEvent
	err := msgpack.Unmarshal(data, &ev)
	return ev, err
}
