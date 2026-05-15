package nsjail

import (
	"fmt"
	"strings"
)

type Mode int

const (
	ModeOnce   Mode = iota
	ModeListen
)

type MountPt struct {
	Src    string
	Dst    string
	Fstype string
	Rw     bool
	IsBind bool
}

type MacvlanConfig struct {
	Iface   string
	IP      string
	Netmask string
	Gateway string
	MAC     string
}

type NsjailConfig struct {
	Mode                Mode
	Chroot              string
	Log                 string
	Hostname            string
	Cwd                 string
	User                string
	TimeLimit           uint32
	DisableCloneNewUser bool
	DisableCloneNewNet  bool
	CloneNewTime        bool
	CgroupMemMax        uint64
	RlimitAS            uint64
	RlimitCore          uint64
	RlimitCPU           uint64
	RlimitFsize         uint64
	RlimitNofile        uint64
	RlimitNproc         uint64
	RlimitStack         uint64
	Envar               []string
	Mounts              []MountPt
	SeccompString       string
	Macvlan             *MacvlanConfig
	UsePasta            bool
	ListenPort          uint16
}

func protoString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '"':
			b.WriteString(`\"`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < 0x20 || c == 0x7F: // NUL and all other ASCII control chars
			fmt.Fprintf(&b, `\%03o`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (c NsjailConfig) ToTextProto() string {
	var b strings.Builder

	switch c.Mode {
	case ModeListen:
		b.WriteString("mode: LISTEN\n")
	default:
		b.WriteString("mode: ONCE\n")
	}

	fmt.Fprintf(&b, "log_file: %s\n", protoString(c.Log))

	if c.Hostname != "" {
		fmt.Fprintf(&b, "hostname: %s\n", protoString(c.Hostname))
	}
	if c.Cwd != "" {
		fmt.Fprintf(&b, "cwd: %s\n", protoString(c.Cwd))
	}
	if c.TimeLimit > 0 {
		fmt.Fprintf(&b, "time_limit: %d\n", c.TimeLimit)
	}
	// clone_newuser/clone_newnet default to true in nsjail's proto.
	// Emit false only when explicitly disabled.
	if c.DisableCloneNewUser {
		b.WriteString("clone_newuser: false\n")
	}
	if c.DisableCloneNewNet {
		b.WriteString("clone_newnet: false\n")
	}
	if c.CloneNewTime {
		b.WriteString("clone_newtime: true\n")
	}
	if c.CgroupMemMax > 0 {
		fmt.Fprintf(&b, "cgroup_mem_max: %d\n", c.CgroupMemMax)
	}
	if c.RlimitAS > 0 {
		fmt.Fprintf(&b, "rlimit_as: %d\n", c.RlimitAS)
	}
	if c.RlimitCore > 0 {
		fmt.Fprintf(&b, "rlimit_core: %d\n", c.RlimitCore)
	}
	if c.RlimitCPU > 0 {
		fmt.Fprintf(&b, "rlimit_cpu: %d\n", c.RlimitCPU)
	}
	if c.RlimitFsize > 0 {
		fmt.Fprintf(&b, "rlimit_fsize: %d\n", c.RlimitFsize)
	}
	if c.RlimitNofile > 0 {
		fmt.Fprintf(&b, "rlimit_nofile: %d\n", c.RlimitNofile)
	}
	if c.RlimitNproc > 0 {
		fmt.Fprintf(&b, "rlimit_nproc: %d\n", c.RlimitNproc)
	}
	if c.RlimitStack > 0 {
		fmt.Fprintf(&b, "rlimit_stack: %d\n", c.RlimitStack)
	}
	if c.User != "" {
		q := protoString(c.User)
		fmt.Fprintf(&b, "uidmap { inside_id: %s outside_id: \"\" count: 1 }\n", q)
		fmt.Fprintf(&b, "gidmap { inside_id: %s outside_id: \"\" count: 1 }\n", q)
	}
	for _, e := range c.Envar {
		fmt.Fprintf(&b, "envar: %s\n", protoString(e))
	}
	// Rootfs bind mount must be first so nsjail pivots into it before
	// applying subsequent mounts (workspace, tmpfs, volumes).
	if c.Chroot != "" {
		b.WriteString("mount {\n")
		fmt.Fprintf(&b, "  src: %s\n", protoString(c.Chroot))
		b.WriteString("  dst: \"/\"\n")
		b.WriteString("  is_bind: true\n")
		b.WriteString("}\n")
	}
	for _, m := range c.Mounts {
		b.WriteString("mount {\n")
		if m.Src != "" {
			fmt.Fprintf(&b, "  src: %s\n", protoString(m.Src))
		}
		fmt.Fprintf(&b, "  dst: %s\n", protoString(m.Dst))
		if m.Fstype != "" {
			fmt.Fprintf(&b, "  fstype: %s\n", protoString(m.Fstype))
		}
		if m.Rw {
			b.WriteString("  rw: true\n")
		}
		if m.IsBind {
			b.WriteString("  is_bind: true\n")
		}
		b.WriteString("}\n")
	}
	if c.SeccompString != "" {
		fmt.Fprintf(&b, "seccomp_string: %s\n", protoString(c.SeccompString))
	}
	if c.Macvlan != nil {
		mv := c.Macvlan
		fmt.Fprintf(&b, "iface_vs: %s\n", protoString(mv.Iface))
		if mv.IP != "" {
			fmt.Fprintf(&b, "iface_vs_ip: %s\n", protoString(mv.IP))
		}
		if mv.Netmask != "" {
			fmt.Fprintf(&b, "iface_vs_nm: %s\n", protoString(mv.Netmask))
		}
		if mv.Gateway != "" {
			fmt.Fprintf(&b, "iface_vs_gw: %s\n", protoString(mv.Gateway))
		}
		if mv.MAC != "" {
			fmt.Fprintf(&b, "iface_vs_ma: %s\n", protoString(mv.MAC))
		}
	}
	if c.UsePasta {
		b.WriteString("use_pasta: true\n")
	}
	if c.ListenPort > 0 {
		fmt.Fprintf(&b, "port: %d\n", c.ListenPort)
	}

	return b.String()
}
