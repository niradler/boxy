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
            "allowedEgressDomains":["s3.amazonaws.com"],
            "ports":[{"hostPort":8080,"guestPort":80,"protocol":"tcp"}],
            "dns":{"nameservers":["1.1.1.1:53"],"queryTimeoutMs":3000},
            "secrets":[{"envVar":"AWS_TOKEN","value":"secret","allowedHosts":["sts.amazonaws.com"]}],
            "rules":[{"direction":"egress","action":"deny","groups":["metadata"]}]
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
	if b.Network == nil || len(b.Network.AllowedEgressDomains) != 1 {
		t.Fatalf("Network: %+v", b.Network)
	}
	if len(b.Network.Ports) != 1 || b.Network.Ports[0].HostPort != 8080 {
		t.Fatalf("Ports: %v", b.Network.Ports)
	}
	if b.Network.DNS == nil || b.Network.DNS.QueryTimeoutMs != 3000 {
		t.Fatalf("DNS: %+v", b.Network.DNS)
	}
	if len(b.Network.Secrets) != 1 || b.Network.Secrets[0].EnvVar != "AWS_TOKEN" {
		t.Fatalf("Secrets: %v", b.Network.Secrets)
	}
	if len(b.Network.Rules) != 1 || b.Network.Rules[0].Action != "deny" {
		t.Fatalf("Rules: %v", b.Network.Rules)
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
