// Package benchenv produces the environment card: the machine-generated
// record that makes a benchmark result reproducible. `rift bench report`
// refuses to run without one, because a number without its environment is
// not a fact.
package benchenv

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Card is the reproducibility record for a benchmark run.
type Card struct {
	GeneratedAt   string `json:"generated_at"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Kernel        string `json:"kernel,omitempty"`
	GoVersion     string `json:"go_version"`
	NumCPU        int    `json:"num_cpu"`
	GOMAXPROCS    int    `json:"gomaxprocs"`
	Compiler      string `json:"compiler"`
	UlimitNoFile  string `json:"ulimit_no_file,omitempty"`
	WSL2          bool   `json:"wsl2"`
	WSL2Detail    string `json:"wsl2_detail,omitempty"`
	CgroupCPU     string `json:"cgroup_cpu_max,omitempty"`
	CgroupMemory  string `json:"cgroup_memory_max,omitempty"`
	GOGC          string `json:"gogc,omitempty"`
	GOMEMLIMIT    string `json:"gomemlimit,omitempty"`
	BuildVersion  string `json:"build_version,omitempty"`
	BuildCommit   string `json:"build_commit,omitempty"`
	BuildVCSDirty bool   `json:"build_vcs_dirty,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

// Probe collects the host environment into a Card. Fields that cannot be
// read on the current platform are left empty with a Note, rather than
// filled with a plausible-looking default.
func Probe() (*Card, error) {
	c := &Card{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		GoVersion:   runtime.Version(),
		NumCPU:      runtime.NumCPU(),
		GOMAXPROCS:  runtime.GOMAXPROCS(0),
		Compiler:    runtime.Compiler,
	}

	// Kernel + OS release from the platform's own file where available.
	if raw, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		c.Kernel = strings.TrimSpace(string(raw))
	}

	// WSL detection: the kernel release string carries "microsoft" on WSL2,
	// and /proc/version names WSL explicitly. A Windows-native build is not
	// WSL2, so the flag stays false there.
	if raw, err := os.ReadFile("/proc/version"); err == nil {
		v := strings.ToLower(string(raw))
		if strings.Contains(v, "microsoft") || strings.Contains(v, "wsl") {
			c.WSL2 = true
			c.WSL2Detail = strings.TrimSpace(string(raw))
		}
	}

	// cgroup v2 limits: these, not NumCPU, are what actually bound a
	// containerized run, and a mismatch against GOMAXPROCS is the classic
	// performance ghost.
	if raw, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		c.CgroupCPU = strings.TrimSpace(string(raw))
	}
	if raw, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		c.CgroupMemory = strings.TrimSpace(string(raw))
	}

	c.GOGC = os.Getenv("GOGC")
	c.GOMEMLIMIT = os.Getenv("GOMEMLIMIT")

	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				c.BuildCommit = s.Value
			case "vcs.modified":
				c.BuildVCSDirty = s.Value == "true"
			}
		}
		c.BuildVersion = bi.Main.Version
	}

	if c.Kernel == "" {
		c.Notes = append(c.Notes, "kernel release unavailable (non-Linux or restricted /proc)")
	}
	if !c.WSL2 && c.OS == "linux" {
		c.Notes = append(c.Notes, "not detected as WSL2")
	}
	if c.OS == "windows" {
		c.Notes = append(c.Notes, "Windows host: no performance claims may be derived from this card")
	}
	return c, nil
}

// JSON renders the card as indented JSON.
func (c *Card) JSON() ([]byte, error) { return json.MarshalIndent(c, "", "  ") }

// Text renders the card for human reading.
func (c *Card) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Environment card (generated %s)\n", c.GeneratedAt)
	fmt.Fprintf(&b, "  os/arch        %s/%s\n", c.OS, c.Arch)
	if c.Kernel != "" {
		fmt.Fprintf(&b, "  kernel         %s\n", c.Kernel)
	}
	fmt.Fprintf(&b, "  go             %s (%s)\n", c.GoVersion, c.Compiler)
	fmt.Fprintf(&b, "  cpus           %d (GOMAXPROCS %d)\n", c.NumCPU, c.GOMAXPROCS)
	fmt.Fprintf(&b, "  wsl2           %v\n", c.WSL2)
	if c.CgroupCPU != "" {
		fmt.Fprintf(&b, "  cgroup cpu.max %s\n", c.CgroupCPU)
	}
	if c.CgroupMemory != "" {
		fmt.Fprintf(&b, "  cgroup mem.max %s\n", c.CgroupMemory)
	}
	if c.GOGC != "" || c.GOMEMLIMIT != "" {
		fmt.Fprintf(&b, "  gc             GOGC=%s GOMEMLIMIT=%s\n", c.GOGC, c.GOMEMLIMIT)
	}
	if c.BuildCommit != "" {
		fmt.Fprintf(&b, "  build          %s (%s, dirty=%v)\n", c.BuildVersion, c.BuildCommit, c.BuildVCSDirty)
	}
	for _, n := range c.Notes {
		fmt.Fprintf(&b, "  note           %s\n", n)
	}
	return b.String()
}
