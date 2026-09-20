package fleetmanager

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DeploymentID, DatabaseURL, PlayFlowURL, PlayFlowAPIKey, ControlURL string
	BuildHash, Region, ComputeSize, PortName                           string
	ProviderVersion, MaxRooms, MinInstances, MaxInstances              int
	AdminToken                                                         string
	SigningKey                                                         []byte
	AllowHTTP                                                          bool
	Mode                                                               string
	Interval                                                           time.Duration

	LaunchTimeout, AllocationTimeout, PrepareTimeout int64
	ReservationTTL, ReconnectSeconds                 int64
	HeartbeatTimeout, LostTimeout                    int64
	IdleSeconds, ProviderPollSeconds                 int64
	MinLifetimeForAdmission                          int64
	MaxQueueAgeSeconds                               float64
}

func FromEnv() (Config, error) {
	c := Config{DeploymentID: env("FLEET_DEPLOYMENT_ID", "default"), DatabaseURL: os.Getenv("FLEET_DATABASE_URL"), PlayFlowURL: env("FLEET_PLAYFLOW_URL", "https://api.computeflow.cloud/api"), PlayFlowAPIKey: os.Getenv("FLEET_PLAYFLOW_API_KEY"), ControlURL: os.Getenv("FLEET_CONTROL_URL"), BuildHash: os.Getenv("FLEET_BUILD_HASH"), Region: os.Getenv("FLEET_REGION"), ComputeSize: env("FLEET_COMPUTE_SIZE", "small"), PortName: env("FLEET_PORT_NAME", "game"), AdminToken: os.Getenv("FLEET_ADMIN_TOKEN"), AllowHTTP: os.Getenv("FLEET_ALLOW_HTTP") == "true", Mode: env("FLEET_MODE", "production"), Interval: time.Second, LaunchTimeout: 120, AllocationTimeout: 180, PrepareTimeout: 20, ReservationTTL: 60, ReconnectSeconds: 90, HeartbeatTimeout: 8, LostTimeout: 60, IdleSeconds: 600, ProviderPollSeconds: 10, MaxQueueAgeSeconds: 2}
	c.MinLifetimeForAdmission = 1800
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("FLEET_DATABASE_URL is required")
	}
	for key, dest := range map[string]*int{"FLEET_PROVIDER_VERSION": &c.ProviderVersion, "FLEET_MAX_ROOMS": &c.MaxRooms, "FLEET_MIN_INSTANCES": &c.MinInstances, "FLEET_MAX_INSTANCES": &c.MaxInstances} {
		value := os.Getenv(key)
		if value == "" && key == "FLEET_MIN_INSTANCES" {
			continue
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return c, fmt.Errorf("%s must be an integer", key)
		}
		*dest = n
	}
	for key, dest := range map[string]*int64{"FLEET_IDLE_SECONDS": &c.IdleSeconds, "FLEET_LAUNCH_TIMEOUT": &c.LaunchTimeout, "FLEET_ALLOCATION_TIMEOUT": &c.AllocationTimeout, "FLEET_PROVIDER_POLL_SECONDS": &c.ProviderPollSeconds, "FLEET_MIN_LIFETIME_FOR_ADMISSION_SECONDS": &c.MinLifetimeForAdmission} {
		if v := os.Getenv(key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return c, fmt.Errorf("invalid %s", key)
			}
			*dest = n
		}
	}
	key, err := base64.RawURLEncoding.DecodeString(os.Getenv("FLEET_SIGNING_KEY"))
	if err != nil {
		return c, fmt.Errorf("FLEET_SIGNING_KEY must be base64url")
	}
	c.SigningKey = key
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.DeploymentID == "" || c.BuildHash == "" || c.Region == "" || c.PlayFlowAPIKey == "" || c.ControlURL == "" {
		return fmt.Errorf("deployment, build, region, PlayFlow credentials and control URL are required")
	}
	if len(c.SigningKey) != 32 || len(c.AdminToken) < 32 {
		return fmt.Errorf("signing key must be 32 bytes and admin token at least 32 characters")
	}
	if c.MaxRooms < 1 || c.MaxRooms > 512 || c.MaxInstances < 1 || c.MaxInstances > 1000 || c.MinInstances < 0 || c.MinInstances > c.MaxInstances || c.ProviderVersion < 1 {
		return fmt.Errorf("invalid capacity or provider build version")
	}
	if c.Interval <= 0 || c.PrepareTimeout < 1 || c.PrepareTimeout > 120 || c.ReservationTTL != 60 || c.ReconnectSeconds < 1 || c.LaunchTimeout < 1 || c.AllocationTimeout < 1 || c.HeartbeatTimeout < 1 || c.LostTimeout <= c.HeartbeatTimeout || c.IdleSeconds < 1 || c.ProviderPollSeconds < 1 || c.MinLifetimeForAdmission < 60 {
		return fmt.Errorf("invalid lifecycle timeouts")
	}
	u, err := url.Parse(c.ControlURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid control URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.AllowHTTP && c.Mode == "mock") {
		return fmt.Errorf("control URL requires HTTPS (HTTP is limited to explicit mock mode)")
	}
	if c.Mode != "production" && c.Mode != "mock" {
		return fmt.Errorf("unknown fleet mode")
	}
	return nil
}
func env(key, fallback string) string {
	if s := strings.TrimSpace(os.Getenv(key)); s != "" {
		return s
	}
	return fallback
}
