package state

import (
	dbm "github.com/cometbft/cometbft-db"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/types"
)

//
// TODO: Remove dependence on all entities exported from this file.
//
// Every entity exported here is dependent on a private entity from the `state`
// package. Currently, these functions are only made available to tests in the
// `state_test` package, but we should not be relying on them for our testing.
// Instead, we should be exclusively relying on exported entities for our
// testing, and should be refactoring exported entities to make them more
// easily testable from outside of the package.
//

const ValSetCheckpointInterval = valSetCheckpointInterval

// UpdateState is an alias for updateState exported from execution.go,
// exclusively and explicitly for testing.
func UpdateState(
	state State,
	blockID types.BlockID,
	header *types.Header,
	resp *abci.ResponseFinalizeBlock,
	validatorUpdates []*types.Validator,
) (State, error) {
	return updateState(state, blockID, header, resp, validatorUpdates)
}

// ValidateValidatorUpdates is an alias for validateValidatorUpdates exported
// from execution.go, exclusively and explicitly for testing.
func ValidateValidatorUpdates(abciUpdates []abci.ValidatorUpdate, params types.ValidatorParams) error {
	return validateValidatorUpdates(abciUpdates, params)
}

// SaveValidatorsInfo is an alias for the private saveValidatorsInfo method in
// store.go, exported exclusively and explicitly for testing.
func SaveValidatorsInfo(db dbm.DB, height, lastHeightChanged int64, valSet *types.ValidatorSet) error {
	stateStore := dbStore{db, StoreOptions{DiscardABCIResponses: false}}
	return stateStore.saveValidatorsInfo(height, lastHeightChanged, valSet)
}

// FindMinBlockRetainHeight is an alias for the private
// findMinBlockRetainHeight method in pruner.go, exported exclusively and
// expicitly for testing.
func (p *Pruner) FindMinRetainHeight() int64 {
	return p.findMinBlockRetainHeight()
}

func (p *Pruner) PruneABCIResToRetainHeight(lastRetainHeight int64) int64 {
	return p.pruneABCIResToRetainHeight(lastRetainHeight)
}

func (p *Pruner) PruneTxIndexerToRetainHeight(lastRetainHeight int64) int64 {
	return p.pruneTxIndexerToRetainHeight(lastRetainHeight)
}

func (p *Pruner) PruneBlockIndexerToRetainHeight(lastRetainHeight int64) int64 {
	return p.pruneBlockIndexerToRetainHeight(lastRetainHeight)
}

func Int64ToBytes(val int64) []byte {
	return int64ToBytes(val)
}

func Int64FromBytes(val []byte) int64 {
	return int64FromBytes(val)
}
