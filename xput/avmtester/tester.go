// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package avmtester

import (
	"errors"
	"fmt"
	"sync"

	stdmath "math"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/consensus/snowstorm"
	"github.com/ava-labs/avalanchego/snow/engine/avalanche"
	"github.com/ava-labs/avalanchego/utils/codec"
	"github.com/ava-labs/avalanchego/utils/crypto"
	"github.com/ava-labs/avalanchego/utils/logging"
	"github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/utils/timer"
	"github.com/ava-labs/avalanchego/utils/wrappers"
	"github.com/ava-labs/avalanchego/vms/avm"
	"github.com/ava-labs/avalanchego/vms/components/avax"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/xput"
)

const (
	defaultMaxOutstandingVtxs = 50
)

var (
	errAmtZero = errors.New("amount must not positive")
)

// TestConfig is the configuration for a throughput test on the X-Chain
type TestConfig struct {
	// NumTxs to send during the test
	NumTxs int

	// The UTXO spent to pay for this test
	avax.UTXOID
	UTXOAmount uint64 `json:"amount"`

	// Controls the UTXO
	Key *crypto.PrivateKeySECP256K1R

	// Frequency of update logs
	LogFreq int

	// Txs per vertex
	BatchSize int
}

// Config for an avmtester.Tester
type Config struct {
	// The consensus engine
	Engine            *avalanche.Transitive
	NetworkID         uint32
	ChainID           ids.ID
	Clock             timer.Clock
	codec             codec.Codec
	Log               logging.Logger
	TxFee             uint64
	AvaxAssetID       ids.ID
	MaxProcessingVtxs int
}

// tester is a holder for keys and UTXOs for the Avalanche DAG.
// tester implementes Tester
type tester struct {
	Config
	lock     *sync.Mutex
	keychain *secp256k1fx.Keychain // Mapping from public address to the SigningKeys
	utxoSet  *UTXOSet              // Mapping from utxoIDs to UTXOs
	// Asset ID --> Balance of this asset held by this wallet
	balance map[[32]byte]uint64
	// Txs that will be issued as part of this test
	txs []*avm.Tx

	// Signalled when there are fewer than the maximum number of processing vertices
	// Its lock is the engine's lock
	processingVtxsCond *sync.Cond

	// Should only be accessed when processingVtxsCond.L is held
	processingVtxs int
}

// NewTester returns a new Tester
func NewTester(config Config) (xput.Tester, error) {
	c := codec.NewDefault()
	errs := wrappers.Errs{}
	errs.Add(
		c.RegisterType(&avm.BaseTx{}),
		c.RegisterType(&avm.CreateAssetTx{}),
		c.RegisterType(&avm.OperationTx{}),
		c.RegisterType(&avm.ImportTx{}),
		c.RegisterType(&avm.ExportTx{}),
		c.RegisterType(&secp256k1fx.TransferInput{}),
		c.RegisterType(&secp256k1fx.MintOutput{}),
		c.RegisterType(&secp256k1fx.TransferOutput{}),
		c.RegisterType(&secp256k1fx.MintOperation{}),
		c.RegisterType(&secp256k1fx.Credential{}),
	)
	config.codec = c
	t := &tester{
		Config:   config,
		keychain: secp256k1fx.NewKeychain(),
		utxoSet:  &UTXOSet{},
		balance:  make(map[[32]byte]uint64),
	}
	t.processingVtxsCond = sync.NewCond(&t.Config.Engine.Ctx.Lock)
	return t, errs.Err
}

// Run the test. Assumes Init has been called.
// Returns after all the txs have been issued.
func (t *tester) Run(configIntf interface{}) (interface{}, error) {
	config, ok := configIntf.(TestConfig)
	if !ok {
		return nil, fmt.Errorf("expected TestConfig but got %T", configIntf)
	}
	t.importKey(config.Key)
	t.addUTXO(&avax.UTXO{
		UTXOID: avax.UTXOID{
			TxID:        config.TxID,
			OutputIndex: config.OutputIndex,
		},
		Asset: avax.Asset{ID: t.AvaxAssetID},
		Out: &secp256k1fx.TransferOutput{
			Amt: config.UTXOAmount,
			OutputOwners: secp256k1fx.OutputOwners{
				Locktime:  0,
				Threshold: 1,
				Addrs:     []ids.ShortID{config.Key.PublicKey().Address()},
			},
		},
	})

	// Generate all the txs
	if err := t.generateTxs(config.NumTxs, t.AvaxAssetID); err != nil {
		return nil, fmt.Errorf("failed to generate txs: %s", err)
	}

	// Issue the txs
	for i := 0; i < config.NumTxs; i++ {
		t.processingVtxsCond.L.Lock()
		for t.processingVtxs > t.MaxProcessingVtxs {
			// Wait until we process some vertices before issuing more
			t.processingVtxsCond.Wait()
		}

		txs, err := t.nextTxs(config.BatchSize)
		if err != nil {
			t.Log.Info("ran out of transactions after issuing %d", i)
			return nil, nil
		}

		snowstormTxs := make([]snowstorm.Tx, len(txs))
		for i, tx := range txs {
			snowstormTxs[i], err = t.Engine.VM.ParseTx(tx.Bytes())
			if err != nil {
				t.processingVtxsCond.L.Unlock()
				return nil, fmt.Errorf("failed to parse tx: %s", err)
			}
		}

		if err := t.Engine.Issue(snowstormTxs); err != nil {
			t.processingVtxsCond.L.Unlock()
			return nil, fmt.Errorf("failed to issue tx: %s", err)
		}
		t.processingVtxsCond.L.Unlock()

		if i == config.NumTxs-1 || (i%config.LogFreq == 0 && i != 0) {
			t.Log.Info("sent %d of %d txs", (i+1)*config.BatchSize, config.NumTxs)
		}
	}

	return nil, nil
}

// Issue is called when the given container is issued to consensus
// Assumes t.processingVtxsCond.L is held
func (t *tester) Issue(ctx *snow.Context, containerID ids.ID, container []byte) error {
	t.processingVtxs++
	return nil
}

// Accept is called when the given container is accepted by consensus
// Assumes t.processingVtxsCond.L is held
func (t *tester) Accept(ctx *snow.Context, containerID ids.ID, container []byte) error {
	t.processingVtxs--
	if t.processingVtxs < t.MaxProcessingVtxs {
		t.processingVtxsCond.Signal()
	}
	return nil
}

// Reject is called when the given container is rejected by consensus
// Assumes t.processingVtxsCond.L is held
func (t *tester) Reject(ctx *snow.Context, containerID ids.ID, container []byte) error {
	t.processingVtxs--
	if t.processingVtxs < t.MaxProcessingVtxs {
		t.processingVtxsCond.Signal()
	}
	return nil
}

// getAddress returns one of the addresses this wallet manages.
// If no address exists, one will be created.
func (t *tester) getAddress() (ids.ShortID, error) {
	if t.keychain.Addrs.Len() == 0 {
		return t.createAddress()
	}
	return t.keychain.Addrs.CappedList(1)[0], nil
}

// createAddress returns a new address.
// It also saves the address and the private key that controls it
// so the address can be used later
func (t *tester) createAddress() (ids.ShortID, error) {
	privKey, err := t.keychain.New()
	return privKey.PublicKey().Address(), err
}

// importKey imports a private key into this wallet
func (t *tester) importKey(sk *crypto.PrivateKeySECP256K1R) { t.keychain.Add(sk) }

// addUTXO adds a new UTXO to this wallet if this wallet may spend it
// The UTXO's output must be an avax.TransferableOut
func (t *tester) addUTXO(utxo *avax.UTXO) {
	out, ok := utxo.Out.(avax.TransferableOut)
	if !ok {
		return
	}

	if _, _, err := t.keychain.Spend(out, stdmath.MaxUint64); err == nil {
		t.utxoSet.Put(utxo)
		t.balance[utxo.AssetID().Key()] += out.Amount()
	}
}

// removeUTXO from this wallet
func (t *tester) removeUTXO(utxoID ids.ID) {
	utxo := t.utxoSet.Get(utxoID)
	if utxo == nil {
		return
	}

	assetID := utxo.AssetID()
	assetKey := assetID.Key()
	newBalance := t.balance[assetKey] - utxo.Out.(avax.TransferableOut).Amount()
	if newBalance == 0 {
		delete(t.balance, assetKey)
	} else {
		t.balance[assetKey] = newBalance
	}

	t.utxoSet.Remove(utxoID)
}

// createTx returns a tx that sends [amount] of [assetID] to [destAddr]
// Any change is sent to an address controlled by this wallet
func (t *tester) createTx(assetID ids.ID, amount uint64, destAddr, changeAddr ids.ShortID, time uint64) (*avm.Tx, error) {
	if amount == 0 {
		return nil, errAmtZero
	}

	amountSpent := uint64(0)

	ins := []*avax.TransferableInput{}
	keys := [][]*crypto.PrivateKeySECP256K1R{}
	for _, utxo := range t.utxoSet.UTXOs {
		if !utxo.AssetID().Equals(assetID) {
			continue
		}
		inputIntf, signers, err := t.keychain.Spend(utxo.Out, time)
		if err != nil {
			continue
		}
		input, ok := inputIntf.(avax.TransferableIn)
		if !ok {
			continue
		}
		spent, err := math.Add64(amountSpent, input.Amount())
		if err != nil {
			return nil, err
		}
		amountSpent = spent

		in := &avax.TransferableInput{
			UTXOID: utxo.UTXOID,
			Asset:  avax.Asset{ID: assetID},
			In:     input,
		}

		ins = append(ins, in)
		keys = append(keys, signers)

		if amountSpent >= amount+t.TxFee {
			break
		}
	}

	if amountSpent < amount+t.TxFee {
		return nil, fmt.Errorf("amount spent (%d) < amount (%d)", amountSpent, amount)
	}

	avax.SortTransferableInputsWithSigners(ins, keys)

	outs := []*avax.TransferableOutput{{
		Asset: avax.Asset{ID: assetID},
		Out: &secp256k1fx.TransferOutput{
			Amt: amount,
			OutputOwners: secp256k1fx.OutputOwners{
				Locktime:  0,
				Threshold: 1,
				Addrs:     []ids.ShortID{destAddr},
			},
		},
	}}

	if amountSpent > amount+t.TxFee {
		outs = append(outs, &avax.TransferableOutput{
			Asset: avax.Asset{ID: assetID},
			Out: &secp256k1fx.TransferOutput{
				Amt: amountSpent - amount - t.TxFee,
				OutputOwners: secp256k1fx.OutputOwners{
					Locktime:  0,
					Threshold: 1,
					Addrs:     []ids.ShortID{changeAddr},
				},
			},
		})
	}

	avax.SortTransferableOutputs(outs, t.codec)

	tx := &avm.Tx{UnsignedTx: &avm.BaseTx{BaseTx: avax.BaseTx{
		NetworkID:    t.NetworkID,
		BlockchainID: t.ChainID,
		Outs:         outs,
		Ins:          ins,
	}}}
	return tx, tx.SignSECP256K1Fx(t.codec, keys)
}

// generateTxs generates transactions that will be sent during the test.
// [numTxs] are generated. Each sends 1 unit of [assetID].
func (t *tester) generateTxs(numTxs int, assetID ids.ID) error {
	t.Log.Info("Generating %d transactions", numTxs)

	frequency := numTxs / 50
	if frequency > 1000 {
		frequency = 1000
	}
	if frequency == 0 {
		frequency = 1
	}

	now := t.Clock.Unix()
	addrs := t.keychain.Addresses().CappedList(1)
	if len(addrs) == 0 {
		return errors.New("keychain has no keys")
	}
	t.txs = make([]*avm.Tx, numTxs)
	for i := 0; i < numTxs; i++ {
		tx, err := t.createTx(assetID, 1, addrs[0], addrs[0], now)
		if err != nil {
			return err
		}

		for _, utxoID := range tx.InputUTXOs() {
			t.removeUTXO(utxoID.InputID())
		}
		for _, utxo := range tx.UTXOs() {
			t.addUTXO(utxo)
		}

		// Periodically log progress
		if numGenerated := i + 1; numGenerated%frequency == 0 {
			t.Log.Info("Generated %d out of %d transactions", numGenerated, numTxs)
		}

		t.txs[i] = tx
	}

	t.Log.Info("Finished generating %d transactions", numTxs)
	return nil
}

// nextTxs returns the next [n] txs to be sent as part of xput test
// If there are less than [n] txs, returns all remaining txs
// Returns error if there are no more transactions
func (t *tester) nextTxs(n int) ([]*avm.Tx, error) {
	if len(t.txs) == 0 {
		return nil, errors.New("no remaining transactions")
	}
	if len(t.txs) < n { // There aren't [n] txs
		return t.txs, nil // Return all remaining txs
	}
	txs := t.txs[:n]
	t.txs = t.txs[n:]
	return txs, nil
}

func (t *tester) String() string {
	return fmt.Sprintf(
		"Keychain:\n"+
			"%s\n"+
			"%s",
		t.keychain.PrefixedString("    "),
		t.utxoSet.PrefixedString("    "),
	)
}
