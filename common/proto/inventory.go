package proto

import "time"

// Inventory is the Slave hardware report (§6.1).
type Inventory struct {
	V           int          `json:"v"`
	CollectedAt time.Time    `json:"collected_at"`
	Host        HostInfo     `json:"host"`
	CPU         CPUInfo      `json:"cpu"`
	Memory      MemoryInfo   `json:"memory"`
	Disks       []DiskInfo   `json:"disks"`
	GPUs        []GPUInfo    `json:"gpus"`
	Accel       AccelSummary `json:"accel_summary"`
	Network     []NetIfInfo  `json:"network"`
}

type HostInfo struct {
	Hostname    string `json:"hostname"`
	OS          string `json:"os"`
	Codename    string `json:"codename,omitempty"` // e.g. noble
	Kernel      string `json:"kernel"`
	Product     string `json:"product"`
	ProductUUID string `json:"product_uuid"`
}

type CPUInfo struct {
	Model   string   `json:"model"`
	Vendor  string   `json:"vendor"`
	Sockets int      `json:"sockets"`
	Cores   int      `json:"cores"`
	Threads int      `json:"threads"`
	Flags   []string `json:"flags"`
}

type MemoryInfo struct {
	Total uint64 `json:"total"`
}

type DiskInfo struct {
	Mount      string `json:"mount"`
	Total      uint64 `json:"total"`
	Free       uint64 `json:"free"`
	Rotational bool   `json:"rotational"`
}

type GPUInfo struct {
	Index  int    `json:"index"`
	Vendor string `json:"vendor"` // nvidia|amd|intel
	Model  string `json:"model"`
	PCIID  string `json:"pci_id"`
	BusID  string `json:"bus_id,omitempty"` // e.g. 0000:01:00.0
	// Modalias is the kernel's ID string for the card; driver packages
	// list the modaliases they support.
	Modalias string     `json:"modalias,omitempty"`
	VRAM     uint64     `json:"vram"`
	Compute  GPUCompute `json:"compute"`
	Driver   GPUDriver  `json:"driver"`
}

type GPUCompute struct {
	API        string `json:"api"` // cuda|rocm|level-zero|none
	Capability string `json:"capability"`
}

type GPUDriver struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
}

type AccelSummary struct {
	Best      string `json:"best"` // cuda|rocm|level-zero|cpu
	TotalVRAM uint64 `json:"total_vram"`
	GPUCount  int    `json:"gpu_count"`
}

type NetIfInfo struct {
	Iface     string   `json:"iface"`
	MAC       string   `json:"mac"`
	SpeedMbps int      `json:"speed_mbps"`
	Addrs     []string `json:"addrs"`
}
