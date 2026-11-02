// Package proto holds the wire types shared by the Controller and the Slave
// agent. docs/protocol.md is the human-readable spec for every type here.
package proto

import "time"

// Version is the protocol major version carried in every message as "v".
const Version = 1

// Default ports and timings (docs/ports.md, docs/protocol.md §5.3).
const (
	DefaultAgentPort      = 7450
	DefaultNodeAPIPort    = 7443
	DefaultMgmtPort       = 8090
	DefaultBeaconPort     = 47800
	SlaveMDNSService      = "_aicluster-slave._tcp"
	ControllerMDNSService = "_aicluster-ctrl._tcp"

	DefaultHeartbeatInterval = 5 * time.Second
	MissedHeartbeatsOffline  = 3
)

// Agent states as the Slave sees itself.
const (
	AgentUnadopted = "unadopted"
	AgentEnrolling = "enrolling"
	AgentAdopted   = "adopted"
)

// Pairing modes advertised in Hello.
const (
	PairingNone  = "none"
	PairingCPace = "cpace"
)

// ErrorBody is the standard error envelope.
type ErrorBody struct {
	V     int       `json:"v"`
	Error ErrorInfo `json:"error"`
}

type ErrorInfo struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Error codes used across APIs.
const (
	ErrAlreadyAdopted       = "already_adopted"
	ErrEnrollmentInProgress = "enrollment_in_progress"
	ErrPairingRequired      = "pairing_required"
	ErrPairingFailed        = "pairing_failed"
	ErrBadMAC               = "bad_mac"
	ErrStaleGeneration      = "stale_generation"
	ErrRevoked              = "revoked"
	ErrUnknownNode          = "unknown_node"
	ErrForbidden            = "forbidden"
	ErrBadRequest           = "bad_request"
	ErrNotFound             = "not_found"
	ErrWrongState           = "wrong_state"
	ErrInternal             = "internal"
)

// Beacon is the UDP discovery payload (§2.2). Kind selects the sender role.
type Beacon struct {
	V               int       `json:"v"`
	Kind            string    `json:"kind"` // "slave_beacon" | "controller_beacon"
	NodeID          string    `json:"node_id,omitempty"`
	State           string    `json:"state,omitempty"`
	Port            int       `json:"port"`
	FP              string    `json:"fp"`
	ClusterID       string    `json:"cluster_id,omitempty"`
	HW              string    `json:"hw,omitempty"`
	PairingRequired bool      `json:"pairing_required,omitempty"`
	AgentVersion    string    `json:"agent_version,omitempty"`
	Addrs           []string  `json:"addrs"`
	TS              time.Time `json:"ts"`
}

const (
	BeaconKindSlave      = "slave_beacon"
	BeaconKindController = "controller_beacon"
)

// Info is returned by GET /v1/info.
type Info struct {
	V            int    `json:"v"`
	NodeID       string `json:"node_id"`
	State        string `json:"state"`
	ClusterID    string `json:"cluster_id,omitempty"`
	AgentVersion string `json:"agent_version"`
	FP           string `json:"fp"`
}

// Hello is returned by GET /v1/enroll/hello (§3.1).
type Hello struct {
	V             int        `json:"v"`
	NodeID        string     `json:"node_id"`
	HWFingerprint string     `json:"hw_fingerprint"`
	NonceS        []byte     `json:"nonce_s"`
	Pairing       string     `json:"pairing"`
	Reenroll      bool       `json:"reenroll,omitempty"`
	Inventory     *Inventory `json:"inventory"`
	AgentVersion  string     `json:"agent_version"`
}

// PakeStart / PakeStartResp / PakeConfirm / PakeConfirmResp implement §3.2.
type PakeStart struct {
	V      int    `json:"v"`
	YA     []byte `json:"ya"`
	NonceC []byte `json:"nonce_c"`
}

type PakeStartResp struct {
	V  int    `json:"v"`
	YB []byte `json:"yb"`
}

type PakeConfirm struct {
	V    int    `json:"v"`
	TagC []byte `json:"tag_c"`
}

type PakeConfirmResp struct {
	V    int    `json:"v"`
	TagS []byte `json:"tag_s"`
}

// CSRRequest / CSRResponse implement §3.3.
type CSRRequest struct {
	V         int    `json:"v"`
	ClusterID string `json:"cluster_id"`
	CACertPEM string `json:"ca_cert_pem"`
	NonceC    []byte `json:"nonce_c"`
	NonceS    []byte `json:"nonce_s"`
	MAC       []byte `json:"mac,omitempty"`
}

type CSRResponse struct {
	V      int    `json:"v"`
	CSRPEM string `json:"csr_pem"`
	MAC    []byte `json:"mac,omitempty"`
}

// ControllerRef tells a Slave how to reach and recognise its Controller.
type ControllerRef struct {
	ClusterID string   `json:"cluster_id"`
	Endpoints []string `json:"endpoints"`
	CertFP    string   `json:"cert_fp"`
}

// EnrollComplete implements §3.4. MAC covers the body with MAC set to nil.
type EnrollComplete struct {
	V                  int           `json:"v"`
	NodeCertPEM        string        `json:"node_cert_pem"`
	CAChainPEM         string        `json:"ca_chain_pem"`
	Controller         ControllerRef `json:"controller"`
	NodeName           string        `json:"node_name"`
	HeartbeatIntervalS int           `json:"heartbeat_interval_s"`
	DesiredState       DesiredState  `json:"desired_state"`
	MAC                []byte        `json:"mac,omitempty"`
}

type OK struct {
	V  int  `json:"v"`
	OK bool `json:"ok"`
}

// DesiredState is the Controller's declaration of what a Slave should run
// (§5.1). OS fields arrive in a later build step.
type DesiredState struct {
	V            int            `json:"v"`
	Generation   int64          `json:"generation"`
	NodeName     string         `json:"node_name"`
	AllowReboot  bool           `json:"allow_reboot"`
	GCGraceS     int            `json:"gc_grace_s,omitempty"`
	AgentVersion string         `json:"agent_version,omitempty"`
	Runtimes     []RuntimeSpec  `json:"runtimes,omitempty"`
	Models       []DesiredModel `json:"models,omitempty"`
	// Network moves the node onto the Controller's own subnet (protocol §12).
	// Absent: the node stays on whatever network it booted on.
	Network *SlaveNetwork `json:"network,omitempty"`
	// Update asks the agent to install a newer build of itself (§13). The
	// Controller sets it on one node at a time during a rolling update.
	Update *AgentUpdate `json:"update,omitempty"`
	// Drivers is the GPU driver the Controller picked for this node's cards
	// (§14). Absent: leave the node's drivers alone.
	Drivers *DriverSpec `json:"drivers,omitempty"`
	// Fans: absent or "firmware" leaves the fans to the board.
	Fans *FanSpec `json:"fans,omitempty"`
}

// DriverSpec names Ubuntu packages for a GPU driver. The agent installs them
// with apt from the Controller's copy of Ubuntu's archive, so Ubuntu's
// signatures are checked on the node itself.
type DriverSpec struct {
	Vendor   string   `json:"vendor"`   // nvidia | intel | amd
	Name     string   `json:"name"`     // e.g. nvidia-driver-580-server-open, mesa-vulkan-drivers
	Version  string   `json:"version"`  // Ubuntu package version
	Packages []string `json:"packages"` // to install, kernel modules included
	// Mirror is the archive's path on the node API, e.g. /apt/ubuntu.
	Mirror     string   `json:"mirror"`
	Suites     []string `json:"suites"`     // e.g. noble noble-updates noble-security
	Components []string `json:"components"` // e.g. main restricted
	// UseIntegrated lets the runtime use integrated GPUs (Intel and AMD
	// graphics built into the CPU), which share the node's RAM.
	UseIntegrated bool `json:"use_integrated,omitempty"`
}

// Driver states in ObservedDriver.State.
const (
	DriverInstalling     = "installing"
	DriverRebooting      = "rebooting"
	DriverRebootRequired = "reboot_required" // installed; waiting for permission to reboot
	DriverReady          = "ready"
	DriverFailed         = "failed"
)

// ObservedDriver reports the agent's progress with DesiredState.Drivers.
type ObservedDriver struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Version string `json:"version,omitempty"` // as the loaded kernel module reports it
	Error   string `json:"error,omitempty"`
}

// AgentUpdate points at the update bundle the Controller serves on its node
// API. The agent verifies SHA256 before installing anything.
type AgentUpdate struct {
	Commit string `json:"commit"`
	Path   string `json:"path"` // on the node API, e.g. /v1/update/bundle
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Agent update states in ObservedUpdate.State.
const (
	UpdateDownloading = "downloading"
	UpdateInstalling  = "installing"
	UpdateVerifying   = "verifying" // running the new build; the updater is checking it stays up
	UpdateFailed      = "failed"
)

// ObservedUpdate reports the agent's progress with DesiredState.Update.
type ObservedUpdate struct {
	Commit string `json:"commit"`
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
}

// SlaveNetwork is the Controller's subnet for Slaves, on the same wire as
// the home LAN. The Controller is its only DHCP and DNS server and answers
// only reserved MACs, so other devices on the wire never see it.
type SlaveNetwork struct {
	Subnet       string `json:"subnet"`        // e.g. 10.10.10.0/24
	ControllerIP string `json:"controller_ip"` // DHCP server, DNS server and node API
	Address      string `json:"address"`       // reserved for this node
	MAC          string `json:"mac"`           // the reservation is for this interface
	Domain       string `json:"domain"`        // DNS zone the Controller serves, e.g. "aicluster"
	NodeAPIPort  int    `json:"node_api_port"`
}

// ObservedNetwork is the interface a node uses to reach the Controller.
type ObservedNetwork struct {
	Iface      string `json:"iface"`
	MAC        string `json:"mac"`
	Address    string `json:"address"`
	OnSlaveNet bool   `json:"on_slave_net"`
	Error      string `json:"error,omitempty"`
}

type RuntimeSpec struct {
	Kind    string            `json:"kind"` // ollama | llamacpp | vllm | whisper
	Version string            `json:"version,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// DesiredModel is one model a node should hold. AssignmentID ties the
// agent's progress reports back to the Controller's assignment.
type DesiredModel struct {
	AssignmentID string      `json:"assignment_id"`
	ModelID      string      `json:"model_id"`
	Variant      string      `json:"variant"` // quantisation, e.g. q4_K_M
	Runtime      string      `json:"runtime"`
	Source       ModelSource `json:"source"`
	Residency    string      `json:"residency"` // resident | on_demand
	Ctx          int         `json:"ctx,omitempty"`
	Size         int64       `json:"size,omitempty"`
	// Role and Pool are set for a model split across nodes: the leader
	// loads the file and serves; workers run llama.cpp's rpc-server and
	// accept connections only from the leader, through the agents' mTLS.
	Role string    `json:"role,omitempty"` // "" | leader | worker
	Pool *PoolSpec `json:"pool,omitempty"`
}

// Pool roles in DesiredModel.Role / ObservedModel.Role.
const (
	RoleLeaderName = "leader"
	RoleWorkerName = "worker"
)

type PoolSpec struct {
	GroupID  string       `json:"group_id"`
	Leader   string       `json:"leader"` // node_id
	Members  []PoolMember `json:"members"`
	Layers   int          `json:"layers,omitempty"` // total layers, when known
	Parallel int          `json:"parallel,omitempty"`
}

type PoolMember struct {
	NodeID   string  `json:"node_id"`
	Endpoint string  `json:"endpoint"` // agent base URL, e.g. https://10.0.0.5:7450
	Share    float64 `json:"share"`    // fraction of the layers
	Leader   bool    `json:"leader,omitempty"`
}

type ModelSource struct {
	Type   string `json:"type"` // "ollama" (OCI pull through the Controller) | "blob"
	Ref    string `json:"ref,omitempty"`
	Digest string `json:"digest,omitempty"`
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

const (
	ResidencyResident = "resident"
	ResidencyOnDemand = "on_demand"
)

// Model states reported by the agent.
const (
	ModelPending     = "pending"
	ModelDownloading = "downloading"
	ModelInstalled   = "installed"
	ModelLoading     = "loading"
	ModelLoaded      = "loaded"
	ModelFailed      = "failed"
)

type ObservedModel struct {
	AssignmentID string  `json:"assignment_id"`
	ModelID      string  `json:"model_id"`
	Variant      string  `json:"variant"`
	Runtime      string  `json:"runtime"`
	LocalName    string  `json:"local_name,omitempty"` // the runtime's name for it
	State        string  `json:"state"`
	Role         string  `json:"role,omitempty"`
	Progress     float64 `json:"progress"`
	BytesDone    int64   `json:"bytes_done"`
	BytesTotal   int64   `json:"bytes_total"`
	Error        string  `json:"error,omitempty"`
}

type AcceptedGeneration struct {
	V                  int   `json:"v"`
	AcceptedGeneration int64 `json:"accepted_generation"`
}

// Condition is one reconcile step's status.
type Condition struct {
	Type   string    `json:"type"`
	Status bool      `json:"status"`
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// ObservedState is reported by the agent (§5.2).
type ObservedState struct {
	ObservedGeneration int64             `json:"observed_generation"`
	Phase              string            `json:"phase"`
	RebootRequired     bool              `json:"reboot_required"`
	Conditions         []Condition       `json:"conditions"`
	Versions           map[string]string `json:"versions"`
	Models             []ObservedModel   `json:"models"`
	Network            *ObservedNetwork  `json:"network,omitempty"`
	Update             *ObservedUpdate   `json:"update,omitempty"`
	Driver             *ObservedDriver   `json:"driver,omitempty"`
	// GPUs is the node's current GPU list. The Controller replaces the
	// inventory's with it, so cards show their memory once a driver loads
	// and removed cards disappear. Always sent, [] when there are none;
	// absent means an agent too old to report it.
	GPUs []GPUInfo `json:"gpus"`
}

// Versions keys besides "agent", "kernel", "os" and "runtime".
const (
	// VersionAgentCommit is the git commit of the running agent.
	VersionAgentCommit = "agent_commit"
	// VersionSelfUpdate is "yes" when the node can install updates, else
	// why not. Older agents don't report it and can't.
	VersionSelfUpdate = "self_update"
	// VersionPeerBlobs is "yes" when the node serves its model files to
	// other nodes (§16).
	VersionPeerBlobs = "peer_blobs"
)

const (
	PhaseProvisioning = "provisioning"
	PhaseReady        = "ready"
	PhaseDegraded     = "degraded"
	PhaseDraining     = "draining"
)

// Health is the lightweight part of every heartbeat.
type Health struct {
	UptimeS   int64         `json:"uptime_s"`
	Load1     float64       `json:"load1"`
	CPUUtil   float64       `json:"cpu_util"`
	MemTotal  uint64        `json:"mem_total"`
	MemAvail  uint64        `json:"mem_avail"`
	DiskFree  uint64        `json:"disk_free"`
	GPUs      []GPUHealth   `json:"gpus"`
	Inference InferenceLoad `json:"inference"`
	// Thermal is absent from agents older than temperature reporting (§15).
	Thermal *Thermal `json:"thermal,omitempty"`
	// Link is the network link the node reaches the Controller over (§16).
	Link *Link `json:"link,omitempty"`
}

// Link describes a network interface's connection (§16).
type Link struct {
	Iface    string `json:"iface"`
	Mbps     int    `json:"mbps"`             // negotiated speed; 0 when unknown
	Duplex   string `json:"duplex,omitempty"` // full | half
	Wireless bool   `json:"wireless,omitempty"`
}

// BlobSource tells a node where to download a model file from (§16):
// another node that already has it, or the Controller when NodeID is empty.
type BlobSource struct {
	V        int    `json:"v"`
	NodeID   string `json:"node_id,omitempty"`
	Endpoint string `json:"endpoint,omitempty"` // the peer agent's base URL
}

// NetTestResult is one Controller-to-node speed test (§16).
type NetTestResult struct {
	ToNodeMbps   float64   `json:"to_node_mbps"`
	FromNodeMbps float64   `json:"from_node_mbps"`
	Bytes        int64     `json:"bytes"`
	At           time.Time `json:"at"`
	Error        string    `json:"error,omitempty"`
}

// Thermal is the node's temperatures and fans (§15).
type Thermal struct {
	// CPUTempC is the hottest CPU package or die; 0 when unknown.
	CPUTempC float64 `json:"cpu_temp_c"`
	// Throttled: the CPU slowed itself down because of heat since the last
	// heartbeat.
	Throttled bool     `json:"throttled"`
	Sensors   []Sensor `json:"sensors,omitempty"`
	Fans      []Fan    `json:"fans,omitempty"`
	// FanControl is "yes" when the agent can drive at least one fan, else
	// why not.
	FanControl string `json:"fan_control"`
}

// Sensor is one temperature reading.
type Sensor struct {
	Chip  string  `json:"chip"` // hwmon driver, e.g. coretemp, k10temp, nvme, amdgpu, nvidia
	Label string  `json:"label"`
	TempC float64 `json:"temp_c"`
	// CritC is the hardware's critical temperature, when it reports one.
	CritC float64 `json:"crit_c,omitempty"`
}

// Fan is one fan the node can see.
type Fan struct {
	ID    string `json:"id"` // e.g. nct6798/fan2
	Label string `json:"label,omitempty"`
	RPM   int    `json:"rpm"`
	// Duty is the PWM output in percent, when readable.
	Duty int `json:"duty,omitempty"`
	// Control: "firmware" (the board runs it), "agent" (boosted by the
	// agent) or "fixed" (no PWM the agent can set).
	Control string `json:"control"`
}

// Fan modes in FanSpec.Mode.
const (
	FansFirmware = "firmware"
	FansBoost    = "boost"
)

// FanSpec tells the agent how to run the node's fans (§15).
type FanSpec struct {
	Mode string `json:"mode"` // firmware | boost
}

type GPUHealth struct {
	Index     int     `json:"index"`
	UUID      string  `json:"uuid"`
	Util      float64 `json:"util"`
	MemUsed   uint64  `json:"mem_used"`
	MemTotal  uint64  `json:"mem_total"`
	TempC     float64 `json:"temp_c"`
	PowerW    float64 `json:"power_w"`
	Throttled bool    `json:"throttled"`
}

type InferenceLoad struct {
	InFlight     int     `json:"in_flight"`
	Slots        int     `json:"slots"`
	Queue        int     `json:"queue"`
	TokensPerS1m float64 `json:"tokens_per_s_1m"`
	Draining     bool    `json:"draining"`
}

// Heartbeat is POSTed by the agent to the Controller (§5.3).
type Heartbeat struct {
	V        int            `json:"v"`
	NodeID   string         `json:"node_id"`
	Seq      uint64         `json:"seq"`
	BootID   string         `json:"boot_id"`
	TS       time.Time      `json:"ts"`
	Addrs    []string       `json:"addrs"`
	Health   Health         `json:"health"`
	Observed *ObservedState `json:"observed,omitempty"`
}

type HeartbeatResp struct {
	V                   int      `json:"v"`
	DesiredGeneration   int64    `json:"desired_generation"`
	CertRenew           bool     `json:"cert_renew"`
	ControllerEndpoints []string `json:"controller_endpoints"`
	HeartbeatIntervalS  int      `json:"heartbeat_interval_s"`
}

// RenewRequest / RenewResponse implement §5.4.
type RenewRequest struct {
	V      int    `json:"v"`
	CSRPEM string `json:"csr_pem"`
}

type RenewResponse struct {
	V           int    `json:"v"`
	NodeCertPEM string `json:"node_cert_pem"`
	CAChainPEM  string `json:"ca_chain_pem"`
}

// ReleaseRequest is the body of POST /v1/ops/release.
type ReleaseRequest struct {
	KeepModels bool `json:"keep_models"`
}

// Job is returned by every mutating op and GET /v1/jobs/{id}.
type Job struct {
	JobID      string     `json:"job_id"`
	Kind       string     `json:"kind"`
	State      string     `json:"state"` // queued|running|succeeded|failed
	Progress   float64    `json:"progress"`
	Message    string     `json:"message"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
