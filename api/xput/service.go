package xput

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ava-labs/avalanchego/api"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/engine/avalanche"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/triggers"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto"
	"github.com/ava-labs/avalanchego/utils/formatting"
	"github.com/ava-labs/avalanchego/utils/json"
	cjson "github.com/ava-labs/avalanchego/utils/json"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/timer"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/xput/avmtester"
	"github.com/gorilla/rpc/v2"
)

const (
	defaultBatchSize         = 10
	defaultMaxProcessingVtxs = 50
)

var errNoKey = errors.New("argument 'key' not given")

type service struct {
	engine      *avalanche.Transitive
	networkID   uint32
	txFee       uint64
	log         logging.Logger
	clock       timer.Clock
	avaxAssetID ids.ID
	factory     *crypto.FactorySECP256K1R
	dispatcher  *triggers.EventDispatcher
}

// NewService returns a new auth API service
func NewService(
	networkID uint32,
	txFee uint64,
	log logging.Logger,
	engine *avalanche.Transitive,
	dispatcher *triggers.EventDispatcher,
) (*common.HTTPHandler, error) {
	newServer := rpc.NewServer()
	codec := cjson.NewCodec()
	newServer.RegisterCodec(codec, "application/json")
	newServer.RegisterCodec(codec, "application/json;charset=UTF-8")
	err := newServer.RegisterService(
		&service{
			engine:      engine,
			networkID:   networkID,
			txFee:       txFee,
			log:         log,
			clock:       timer.Clock{},
			avaxAssetID: engine.Context().AVAXAssetID,
			factory:     &crypto.FactorySECP256K1R{},
			dispatcher:  dispatcher,
		},
		"xput",
	)
	return &common.HTTPHandler{Handler: newServer}, err
}

// RunArgs ...
type RunArgs struct {
	// Number of txs to issue in the test
	NumTxs json.Uint64 `json:"numTxs"`

	// UTXO to spend to pay for txs
	TxID        ids.ID      `json:"txID"`
	OutputIndex json.Uint32 `json:"outputIndex"`
	Amount      json.Uint64 `json:"amount"`

	// Controls the UTXO.
	// CB58 repr. of a private key on the X-Chain
	Key string `json:"key"`

	BatchSize json.Uint64 `json:"batchSize"`

	MaxProcessingVtxs json.Uint64 `json:"maxProcessingVtxs"`
}

// Run a throughput test. Only supports X-Chain right now.
func (s *service) Run(_ *http.Request, args *RunArgs, reply *api.SuccessResponse) error {
	s.log.Info("xput.run called")

	if args.MaxProcessingVtxs == 0 {
		args.MaxProcessingVtxs = defaultMaxProcessingVtxs
	}

	// Create the tester
	t, err := avmtester.NewTester(avmtester.Config{
		Engine:            s.engine,
		NetworkID:         s.networkID,
		ChainID:           s.engine.Context().ChainID,
		Clock:             s.clock,
		Log:               s.log,
		TxFee:             s.txFee,
		AvaxAssetID:       s.avaxAssetID,
		MaxProcessingVtxs: int(args.MaxProcessingVtxs),
	})
	if err != nil {
		return fmt.Errorf("couldn't create new tester: %w", err)
	}
	if err := s.dispatcher.Register("xput", t); err != nil {
		return fmt.Errorf("couldn't register xput test with the event dispatcher: %w", err)
	}
	defer func() {
		if err := s.dispatcher.Deregister("xput"); err != nil {
			s.log.Warn("couldn't deregister xput service from event dispatcher: %s", err)
		}
	}()

	// Parse key (e.g. "PrivateKey-29WjzjTN6ZEvm2mFqzJQkBU15LPsKXKhSYC2TbZrqoFR5ZkBLT")
	if args.Key == "" {
		return errNoKey
	}
	if !strings.HasPrefix(args.Key, constants.SecretKeyPrefix) {
		return fmt.Errorf("private key missing %s prefix", constants.SecretKeyPrefix)
	}
	args.Key = strings.TrimPrefix(args.Key, constants.SecretKeyPrefix)
	keyBytes := formatting.CB58{}
	if err := keyBytes.FromString(args.Key); err != nil {
		return fmt.Errorf("couldn't parse key to bytes: %w", err)
	}
	keyIntf, err := s.factory.ToPrivateKey(keyBytes.Bytes)
	if err != nil {
		return fmt.Errorf("couldn't parse key: %w", err)
	}
	key, ok := keyIntf.(*crypto.PrivateKeySECP256K1R)
	if !ok {
		return fmt.Errorf("expected *crypto.PrivateKeySECP256K1R but got %T", key)
	}

	logFreq := int(args.NumTxs) / 50
	if logFreq > 1000 {
		logFreq = 1000
	}
	if logFreq == 0 {
		logFreq = 1
	}

	if args.BatchSize == 0 {
		args.BatchSize = defaultBatchSize
	}

	// Run the test
	_, err = t.Run(avmtester.TestConfig{
		NumTxs: int(args.NumTxs),
		UTXOID: avax.UTXOID{
			TxID:        args.TxID,
			OutputIndex: uint32(args.OutputIndex),
		},
		LogFreq:    logFreq,
		UTXOAmount: uint64(args.Amount),
		Key:        key,
		BatchSize:  int(args.BatchSize),
	})
	if err != nil {
		return fmt.Errorf("couldn't run xput test: %w", err)
	}

	reply.Success = true
	return nil
}
