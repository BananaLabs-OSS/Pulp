package main

import "testing"

func TestApplicationArgs(t *testing.T) {
	manifestPath, requests, enabled, err := applicationArgs([]string{
		"-manifest", "quota.cell.toml",
		"-request", "40,2,50",
		"-request=-10,3,-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || manifestPath != "quota.cell.toml" || len(requests) != 2 {
		t.Fatalf("unexpected parse: enabled=%t manifest=%q requests=%v", enabled, manifestPath, requests)
	}
	if requests[0].Current != 40 || requests[0].Delta != 2 || requests[0].Limit != 50 || requests[0].Subject != "tenant-a" ||
		requests[1].Current != -10 || requests[1].Delta != 3 || requests[1].Limit != -5 {
		t.Fatalf("unexpected requests: %v", requests)
	}
}

func TestApplicationArgsLeavesNormalRuntimeFlagsAlone(t *testing.T) {
	_, _, enabled, err := applicationArgs([]string{"-manifest", "quota.cell.toml"})
	if err != nil || enabled {
		t.Fatalf("normal runtime args were intercepted: enabled=%t err=%v", enabled, err)
	}
}

func TestApplicationArgsRejectsMalformedRequest(t *testing.T) {
	_, _, _, err := applicationArgs([]string{"-manifest", "quota.cell.toml", "-request", "40,2"})
	if err == nil {
		t.Fatal("malformed request accepted")
	}
}
