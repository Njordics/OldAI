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
// (§5.1). Driver and OS fields arrive in later build steps.
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
}

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
