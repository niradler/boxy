package nsjail

import (
	"strings"
	"testing"
)

func baseConfig() NsjailConfig {
	return NsjailConfig{
		Mode:                ModeOnce,
		Chroot:              "/rootfs/ubuntu-24.04",
		Log:                 "/dev/null",
		DisableCloneNewUser: true,
		DisableCloneNewNet:  true,
	}
}

func TestToTextProto_RequiredFields(t *testing.T) {
	proto := baseConfig().ToTextProto()
	for _, want := range []string{
		"mode: ONCE",
		`chroot: "/rootfs/ubuntu-24.04"`,
		`log: "/dev/null"`,
		"disable_clone_newuser: true",
		"disable_clone_newnet: true",
	} {
		if !strings.Contains(proto, want) {
			t.Errorf("missing %q in:\n%s", want, proto)
		}
	}
}

func TestToTextProto_OptionalFields(t *testing.T) {
	cfg := baseConfig()
	cfg.Hostname = "sandbox-1"
	cfg.Cwd = "/workspace"
	cfg.TimeLimit = 30
	cfg.CgroupMemMax = 512 * 1024 * 1024
	cfg.RlimitNofile = 1024
	proto := cfg.ToTextProto()
	for _, want := range []string{
		`hostname: "sandbox-1"`,
		`cwd: "/workspace"`,
		"time_limit: 30",
		"cgroup_mem_max: 536870912",
		"rlimit_nofile: 1024",
	} {
		if !strings.Contains(proto, want) {
			t.Errorf("missing %q in:\n%s", want, proto)
		}
	}
}

func TestToTextProto_UserAsUidmapGidmap(t *testing.T) {
	cfg := baseConfig()
	cfg.User = "nobody"
	proto := cfg.ToTextProto()
	if !strings.Contains(proto, `uidmap { inside_id: "nobody"`) {
		t.Errorf("missing uidmap in:\n%s", proto)
	}
	if !strings.Contains(proto, `gidmap { inside_id: "nobody"`) {
		t.Errorf("missing gidmap in:\n%s", proto)
	}
}

func TestToTextProto_BindMount(t *testing.T) {
	cfg := baseConfig()
	cfg.Mounts = append(cfg.Mounts, MountPt{
		Src: "/sandbox/ws", Dst: "/workspace", Rw: true, IsBind: true,
	})
	proto := cfg.ToTextProto()
	for _, want := range []string{
		`src: "/sandbox/ws"`,
		`dst: "/workspace"`,
		"rw: true",
		"is_bind: true",
	} {
		if !strings.Contains(proto, want) {
			t.Errorf("missing %q in:\n%s", want, proto)
		}
	}
}

func TestToTextProto_TmpfsMount(t *testing.T) {
	cfg := baseConfig()
	cfg.Mounts = append(cfg.Mounts, MountPt{
		Dst: "/tmp", Fstype: "tmpfs", Rw: true,
	})
	proto := cfg.ToTextProto()
	if !strings.Contains(proto, `fstype: "tmpfs"`) {
		t.Errorf("missing fstype in:\n%s", proto)
	}
	if strings.Contains(proto, "is_bind: true") {
		t.Errorf("unexpected is_bind in tmpfs mount:\n%s", proto)
	}
}

func TestToTextProto_EscapesSpecialChars(t *testing.T) {
	cfg := baseConfig()
	cfg.Envar = append(cfg.Envar, `KEY=val"ue`)
	proto := cfg.ToTextProto()
	if !strings.Contains(proto, `envar: "KEY=val\"ue"`) {
		t.Errorf("missing escaped envar in:\n%s", proto)
	}
}

func TestToTextProto_SeccompString(t *testing.T) {
	cfg := baseConfig()
	cfg.SeccompString = "POLICY foo {\nALLOW { read }\n}"
	proto := cfg.ToTextProto()
	if !strings.Contains(proto, "seccomp_string:") {
		t.Errorf("missing seccomp_string in:\n%s", proto)
	}
	if !strings.Contains(proto, `POLICY foo {\nALLOW`) {
		t.Errorf("missing escaped seccomp body in:\n%s", proto)
	}
}

func TestToTextProto_MacvlanConfig(t *testing.T) {
	cfg := baseConfig()
	cfg.Macvlan = &MacvlanConfig{
		Iface:   "eth0",
		IP:      "10.0.0.2",
		Netmask: "255.255.255.0",
		Gateway: "10.0.0.1",
	}
	proto := cfg.ToTextProto()
	for _, want := range []string{
		`iface_vs: "eth0"`,
		`iface_vs_ip: "10.0.0.2"`,
		`iface_vs_nm: "255.255.255.0"`,
		`iface_vs_gw: "10.0.0.1"`,
	} {
		if !strings.Contains(proto, want) {
			t.Errorf("missing %q in:\n%s", want, proto)
		}
	}
	if strings.Contains(proto, "iface_vs_ma:") {
		t.Errorf("unexpected iface_vs_ma (MAC not set):\n%s", proto)
	}
}

func TestToTextProto_PastaAndCloneNewTime(t *testing.T) {
	cfg := baseConfig()
	cfg.UsePasta = true
	cfg.CloneNewTime = true
	proto := cfg.ToTextProto()
	if !strings.Contains(proto, "use_pasta: true") {
		t.Errorf("missing use_pasta in:\n%s", proto)
	}
	if !strings.Contains(proto, "clone_newtime: true") {
		t.Errorf("missing clone_newtime in:\n%s", proto)
	}
}

func TestToTextProto_AbsentOptionalFields(t *testing.T) {
	proto := baseConfig().ToTextProto()
	for _, absent := range []string{
		"hostname:", "seccomp_string:", "use_pasta:", "clone_newtime:", "iface_vs:",
	} {
		if strings.Contains(proto, absent) {
			t.Errorf("unexpected %q in base config:\n%s", absent, proto)
		}
	}
}

func TestToTextProto_ListenMode(t *testing.T) {
	cfg := baseConfig()
	cfg.Mode = ModeListen
	cfg.ListenPort = 8080
	proto := cfg.ToTextProto()
	if !strings.Contains(proto, "mode: LISTEN") {
		t.Errorf("missing LISTEN mode in:\n%s", proto)
	}
	if !strings.Contains(proto, "port: 8080") {
		t.Errorf("missing port in:\n%s", proto)
	}
}
