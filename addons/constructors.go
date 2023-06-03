package addons

import (
	"github.com/rocket-pool/smartnode/addons/graffiti_wall_writer"
	"github.com/rocket-pool/smartnode/addons/rocketarb"
	"github.com/rocket-pool/smartnode/shared/types/addons"
)

func NewGraffitiWallWriter() addons.SmartnodeAddon {
	return graffiti_wall_writer.NewGraffitiWallWriter()
}

func NewRocketArb() addons.SmartnodeAddon {
  return rocketarb.NewRocketArb()
}
