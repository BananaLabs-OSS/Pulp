package main

import "testing"

func TestParseArgsPreservesOpaqueRequests(t *testing.T) {
	options, err := parseArgs([]string{"-manifest", "cell.toml", "-provider=seme.function-v1", "-request", "ff00"})
	if err != nil {
		t.Fatal(err)
	}
	if options.manifest != "cell.toml" || options.provider != "seme.function-v1" || len(options.requests) != 1 || len(options.requests[0]) != 2 || options.requests[0][0] != 0xff {
		t.Fatalf("unexpected options: %#v", options)
	}
}

func TestParseArgsRejectsNonHexRequest(t *testing.T) {
	if _, err := parseArgs([]string{"-manifest=x", "-provider=y", "-request=not-hex"}); err == nil {
		t.Fatal("non-hexadecimal request accepted")
	}
}
