package xput

import (
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
)

// Tester runs throughput tests
type Tester interface {
	// Run the test and return results (e.g. statistics)
	Run(config interface{}) (interface{}, error)

	// Issue is called when the given container is issued to consensus
	Issue(ctx *snow.Context, containerID ids.ID, container []byte) error

	// Accept is called when the given container is accepted by consensus
	Accept(ctx *snow.Context, containerID ids.ID, container []byte) error

	// Reject is called when the given container is rejected by consensus
	Reject(ctx *snow.Context, containerID ids.ID, container []byte) error
}
