package rocketarb

import (
	"fmt"

	"github.com/rocket-pool/smartnode/shared/types/addons"
	cfgtypes "github.com/rocket-pool/smartnode/shared/types/config"
)

const (
	ContainerID_RocketArb  cfgtypes.ContainerID = "rocketarb"
	RocketArbContainerName string               = "addon_rocketarb"
)

type RocketArb struct {
	cfg *RocketArbConfig `yaml:"config,omitempty"`
}

func NewRocketArb() addons.SmartnodeAddon {
	return &RocketArb{
		cfg: NewConfig(),
	}
}

func (ra *RocketArb) GetName() string {
	return "RocketArb"
}

func (ra *RocketArb) GetDescription() string {
  return "TODO: This text should give a description of what the RocketArb addon is for."
}

func (ra *RocketArb) GetConfig() cfgtypes.Config {
	return ra.cfg
}

func (ra *RocketArb) GetContainerName() string {
	return fmt.Sprint(ContainerID_RocketArb)
}

func (ra *RocketArb) GetEnabledParameter() *cfgtypes.Parameter {
	return &ra.cfg.Enabled
}

func (ra *RocketArb) GetContainerTag() string {
	return containerTag
}

func (ra *RocketArb) UpdateEnvVars(envVars map[string]string) error {
	if ra.cfg.Enabled.Value == true {
		cfgtypes.AddParametersToEnvVars(ra.cfg.GetParameters(), envVars)
	}
	return nil
}
