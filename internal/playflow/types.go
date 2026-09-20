// Package playflow implements the public PlayFlow v3 game-server API.
// Fleet state and retry policy belong to the caller, not this HTTP adapter.
package playflow

// StartRequest follows POST /api/v3/servers/start. Version pins an exact build;
// when zero, PlayFlow resolves VersionTag (or its default tag). TTL is seconds.
// Credentials should be passed in EnvironmentVariables, never CustomData:
// CustomData is visible in ordinary server listing and dashboard responses.
type StartRequest struct {
	Name                 string            `json:"name"`
	Region               string            `json:"region"`
	StartupArgs          string            `json:"startup_args,omitempty"`
	ComputeSize          string            `json:"compute_size,omitempty"`
	VersionTag           string            `json:"version_tag,omitempty"`
	Version              int               `json:"version,omitempty"`
	TTL                  int               `json:"ttl,omitempty"`
	AutoRestart          bool              `json:"auto_restart"`
	CustomData           map[string]any    `json:"custom_data,omitempty"`
	MatchID              string            `json:"match_id,omitempty"`
	EnvironmentVariables map[string]string `json:"environment_variables,omitempty"`
	PortConfigs          []PortConfig      `json:"port_configs,omitempty"`
}

// PortConfig overrides a build's configured ports for a particular instance.
type PortConfig struct {
	Name         string `json:"name"`
	InternalPort int    `json:"internal_port"`
	Protocol     string `json:"protocol"`
	TLSEnabled   bool   `json:"tls_enabled"`
}

// Port is the actual public mapping returned by PlayFlow. Clients connect to
// Host:ExternalPort; InternalPort is only the port inside the game container.
type Port struct {
	Name         string `json:"name"`
	Host         string `json:"host"`
	ExternalPort int    `json:"external_port"`
	InternalPort int    `json:"internal_port"`
	Protocol     string `json:"protocol"`
	TLSEnabled   bool   `json:"tls_enabled"`
}

// Instance is the public server representation. Nullable integer fields decode
// to zero. Environment variables are deliberately absent: PlayFlow does not
// expose them in normal instance responses.
type Instance struct {
	InstanceID    string         `json:"instance_id"`
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	NetworkPorts  []Port         `json:"network_ports"`
	StartupArgs   string         `json:"startup_args"`
	ServiceType   string         `json:"service_type"`
	ComputeSize   string         `json:"compute_size"`
	Region        string         `json:"region"`
	VersionTag    string         `json:"version_tag"`
	Version       int            `json:"version"`
	StartedAt     string         `json:"started_at"`
	StoppedAt     string         `json:"stopped_at"`
	AutoRestart   bool           `json:"auto_restart"`
	CustomData    map[string]any `json:"custom_data"`
	TTL           int            `json:"ttl"`
	IsPoolServer  bool           `json:"is_pool_server"`
	PoolClaimedAt string         `json:"pool_claimed_at"`
	MatchID       string         `json:"match_id"`
	CreatedAt     string         `json:"created_at"`
	UpdatedAt     string         `json:"updated_at"`
}

type listResponse struct {
	Total   int        `json:"total"`
	Servers []Instance `json:"servers"`
	Limit   int        `json:"limit"`
	Offset  int        `json:"offset"`
	HasMore *bool      `json:"has_more"`
}
