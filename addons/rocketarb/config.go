package graffiti_wall_writer

import (
	"github.com/rocket-pool/smartnode/shared/types/config"
)

// Constants
const (
	containerTag string = "rocketpool/rocketarb:v0.0.1"
)

// Configuration for RocketArb
type RocketArbConfig struct {
	Title string `yaml:"-"`

	Enabled config.Parameter `yaml:"enabled,omitempty"`

	// The Docker Hub tag
	ContainerTag config.Parameter `yaml:"containerTag,omitempty"`

	// Custom command line flags
	AdditionalFlags config.Parameter `yaml:"additionalFlags,omitempty"`
}

// Creates a new configuration instance
func NewConfig() *RocketArbConfig {
	return &RocketArbConfig{
		Title: "RocketArb Settings",

		Enabled: config.Parameter{
			ID:                   "enabled",
			Name:                 "Enabled",
			Description:          "Enable RocketArb",
			Type:                 config.ParameterType_Bool,
			Default:              map[config.Network]interface{}{config.Network_All: false},
			AffectsContainers:    []config.ContainerID{ContainerID_RocketArb},
			EnvironmentVariables: []string{"ADDON_ROCKETARB_ENABLED"},
			CanBeBlank:           false,
			OverwriteOnUpgrade:   false,
		},

		ContainerTag: config.Parameter{
			ID:                   "containerTag",
			Name:                 "Container Tag",
			Description:          "The tag name of the container you want to use on Docker Hub.",
			Type:                 config.ParameterType_String,
			Default:              map[config.Network]interface{}{config.Network_All: containerTag},
			AffectsContainers:    []config.ContainerID{ContainerID_RocketArb},
			EnvironmentVariables: []string{"ADDON_ROCKETARB_CONTAINER_TAG"},
			CanBeBlank:           false,
			OverwriteOnUpgrade:   true,
		},

		AdditionalFlags: config.Parameter{
			ID:                   "additionalFlags",
			Name:                 "Additional Flags",
			Description:          "Additional custom command line flags you want to pass to the addon, to take advantage of other settings that the Smartnode's configuration doesn't cover.",
			Type:                 config.ParameterType_String,
			Default:              map[config.Network]interface{}{config.Network_All: ""},
			AffectsContainers:    []config.ContainerID{ContainerID_RocketArb},
			EnvironmentVariables: []string{"ADDON_ROCKETARB_ADDITIONAL_FLAGS"},
			CanBeBlank:           true,
			OverwriteOnUpgrade:   false,
		},
	}
}

// Get the parameters for this config
func (cfg *RocketArbConfig) GetParameters() []*config.Parameter {
	return []*config.Parameter{
		&cfg.Enabled,
		&cfg.ContainerTag,
		&cfg.AdditionalFlags,
	}
}

// The the title for the config
func (cfg *RocketArbConfig) GetConfigTitle() string {
	return cfg.Title
}
