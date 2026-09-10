package abi

import "github.com/vmihailenco/msgpack/v5"

const (
	// SSETransportVersionV2 identifies subscriber-aware route registration and
	// event emission. SSEAuthRequestVersionV2 and SSEBindingVersionV2 identify
	// the two halves of the host-to-owner authorization exchange. Keeping these
	// wire versions distinct prevents a transport request from being accepted as
	// an authorization request.
	SSETransportVersionV2   = "pulp.sse.v2"
	SSEAuthRequestVersionV2 = "pulp.sse.auth.v2"
	SSEBindingVersionV2     = "pulp.sse.binding.v2"

	SSEAudiencePublic  = "public"
	SSEAudienceAccount = "account"
	SSEAudiencePool    = "pool"
	SSEAuthProviderV2  = "transport.sse.subscription.authorize.v2"
)

type SSERegisterRequestV2 struct {
	Version     string `msgpack:"version"`
	Pattern     string `msgpack:"pattern"`
	Audience    string `msgpack:"audience"`
	MaxClients  uint32 `msgpack:"max_clients"`
	QueueDepth  uint32 `msgpack:"queue_depth"`
	ReplayLimit uint32 `msgpack:"replay_limit"`
}

type SSEAuthRequestV2 struct {
	Version       string `msgpack:"version"`
	Audience      string `msgpack:"audience"`
	Method        string `msgpack:"method"`
	Path          string `msgpack:"path"`
	Query         string `msgpack:"query"`
	Authorization string `msgpack:"authorization,omitempty"`
	Cookie        string `msgpack:"cookie,omitempty"`
	LastEventID   string `msgpack:"last_event_id,omitempty"`
}

type SSEAuthResultV2 struct {
	Version            string             `msgpack:"version"`
	Audience           string             `msgpack:"audience"`
	Binding            string             `msgpack:"binding"`
	Topic              string             `msgpack:"topic,omitempty"`
	Cursor             uint64             `msgpack:"cursor,omitempty"`
	ExpiresAtUnixMilli uint64             `msgpack:"expires_at_unix_milli"`
	Replay             []SSEReplayEventV2 `msgpack:"replay,omitempty"`
	Resync             bool               `msgpack:"resync,omitempty"`
}

type SSEReplayEventV2 struct {
	ID    string `msgpack:"id"`
	Event string `msgpack:"event,omitempty"`
	Data  string `msgpack:"data"`
}

type SSEEmitRequestV2 struct {
	Version  string `msgpack:"version"`
	Audience string `msgpack:"audience"`
	Binding  string `msgpack:"binding"`
	Topic    string `msgpack:"topic,omitempty"`
	ID       string `msgpack:"id,omitempty"`
	Event    string `msgpack:"event,omitempty"`
	Data     string `msgpack:"data"`
}

func DecodeSSERegisterRequestV2(data []byte) (SSERegisterRequestV2, error) {
	var r SSERegisterRequestV2
	err := msgpack.Unmarshal(data, &r)
	return r, err
}
func EncodeSSEAuthRequestV2(r SSEAuthRequestV2) ([]byte, error) { return msgpack.Marshal(r) }
func DecodeSSEAuthResultV2(data []byte) (SSEAuthResultV2, error) {
	var r SSEAuthResultV2
	err := msgpack.Unmarshal(data, &r)
	return r, err
}
func DecodeSSEEmitRequestV2(data []byte) (SSEEmitRequestV2, error) {
	var r SSEEmitRequestV2
	err := msgpack.Unmarshal(data, &r)
	return r, err
}

// SSEEmitRequest is what the cell passes to sse_emit. Path selects the
// SSE route to broadcast on; Data is the event payload. ID and Event are
// optional SSE fields — ID sets "id:", Event sets "event:" before data.
type SSEEmitRequest struct {
	Path  string `msgpack:"path"`
	ID    string `msgpack:"id,omitempty"`
	Event string `msgpack:"event,omitempty"`
	Data  string `msgpack:"data"`
}

// DecodeSSEEmitRequest parses sse_emit input.
func DecodeSSEEmitRequest(data []byte) (SSEEmitRequest, error) {
	var r SSEEmitRequest
	err := msgpack.Unmarshal(data, &r)
	return r, err
}
