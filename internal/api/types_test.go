package api_test

import (
	"encoding/json"
	"testing"

	"boxy.dev/boxy/internal/api"
)

func TestSandboxCreateBody_FullVMSurface(t *testing.T) {
	body := `{
        "sandboxId":"s1","sessionId":"sess1","owner":"o1",
        "allowedBinaries":["curl","aws"],
        "vm":{
            "memoryMb":1024,"vcpus":2,
            "workdir":"/app","shell":"/bin/bash",
            "hostname":"sandbox-1","user":"nobody",
            "maxDurationSec":3600,"idleTimeoutSec":300,
            "rlimits":[{"resource":"nofile","soft":1024,"hard":4096}],
            "scripts":[{"name":"setup","content":"#!/bin/sh\necho ready"}]
        },
        "network":{
            "allowInternetAccess":true,
            "macvlan":{"interface":"eth0","ip":"10.0.0.2"}
        },
        "volumes":[{"guestPath":"/data","type":"tmpfs","sizeMb":512}],
        "patches":[{"type":"text","path":"/etc/app.conf","content":"key=val","mode":420}]
    }`
	var b api.SandboxCreateBody
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatal(err)
	}
	if b.VM == nil || b.VM.MemoryMB != 1024 {
		t.Fatalf("VM: %+v", b.VM)
	}
	if len(b.VM.Rlimits) != 1 || b.VM.Rlimits[0].Resource != "nofile" {
		t.Fatalf("Rlimits: %v", b.VM.Rlimits)
	}
	if len(b.VM.Scripts) != 1 || b.VM.Scripts[0].Name != "setup" {
		t.Fatalf("Scripts: %v", b.VM.Scripts)
	}
	if !b.Network.AllowInternetAccess {
		t.Fatal("AllowInternetAccess must be true")
	}
	if b.Network.Macvlan == nil || b.Network.Macvlan.Interface != "eth0" || b.Network.Macvlan.IP != "10.0.0.2" {
		t.Fatalf("Macvlan: %+v", b.Network.Macvlan)
	}
	if len(b.Volumes) != 1 || b.Volumes[0].Type != "tmpfs" {
		t.Fatalf("Volumes: %v", b.Volumes)
	}
	if len(b.Patches) != 1 || b.Patches[0].Type != "text" {
		t.Fatalf("Patches: %v", b.Patches)
	}
	if len(b.AllowedBinaries) != 2 {
		t.Fatalf("AllowedBinaries: %v", b.AllowedBinaries)
	}
}
