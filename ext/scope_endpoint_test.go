package ext

import "testing"

type endpointReporterTestDouble struct {
	ready Endpoint
	gone  Endpoint
}

func (r *endpointReporterTestDouble) Ready(endpoint Endpoint) error {
	r.ready = endpoint
	return nil
}

func (r *endpointReporterTestDouble) Gone(endpoint Endpoint) { r.gone = endpoint }

func TestEndpointCarriesScopedCapabilityIdentity(t *testing.T) {
	scope, err := NewScope("sessions", "blue", "api", "default")
	if err != nil {
		t.Fatal(err)
	}
	reporter := &endpointReporterTestDouble{}
	env := SetupEnv{Endpoints: reporter}
	endpoint := Endpoint{
		Scope:      scope,
		Capability: "transport.http.inbound",
		Name:       "public",
		Address:    "127.0.0.1:41000",
	}
	if err := env.Endpoints.Ready(endpoint); err != nil {
		t.Fatal(err)
	}
	env.Endpoints.Gone(endpoint)
	if reporter.ready != endpoint || reporter.gone != endpoint {
		t.Fatalf("endpoint reporter lifecycle = %#v %#v, want %#v", reporter.ready, reporter.gone, endpoint)
	}
}
