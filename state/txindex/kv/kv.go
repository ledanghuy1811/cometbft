package kv

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	dbm "github.com/cometbft/cometbft-db"
	"github.com/gogo/protobuf/proto"

	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/libs/pubsub/query"
	"github.com/tendermint/tendermint/state/indexer"
	"github.com/tendermint/tendermint/state/txindex"
	"github.com/tendermint/tendermint/types"
)

const (
	tagKeySeparator   = "/"
	eventSeqSeparator = "$es$"
)

var _ txindex.TxIndexer = (*TxIndex)(nil)

// TxIndex is the simplest possible indexer, backed by key-value storage (levelDB).
type TxIndex struct {
	store dbm.DB
	// Number the events in the event list
	eventSeq int64
}

// NewTxIndex creates new KV indexer.
func NewTxIndex(store dbm.DB) *TxIndex {
	return &TxIndex{
		store: store,
	}
}

// Get gets transaction from the TxIndex storage and returns it or nil if the
// transaction is not found.
func (txi *TxIndex) Get(hash []byte) (*abci.TxResult, error) {
	if len(hash) == 0 {
		return nil, txindex.ErrorEmptyHash
	}

	rawBytes, err := txi.store.Get(hash)
	if err != nil {
		panic(err)
	}
	if rawBytes == nil {
		return nil, nil
	}

	txResult := new(abci.TxResult)
	err = proto.Unmarshal(rawBytes, txResult)
	if err != nil {
		return nil, fmt.Errorf("error reading TxResult: %v", err)
	}

	return txResult, nil
}

// AddBatch indexes a batch of transactions using the given list of events. Each
// key that indexed from the tx's events is a composite of the event type and
// the respective attribute's key delimited by a "." (eg. "account.number").
// Any event with an empty type is not indexed.
func (txi *TxIndex) AddBatch(b *txindex.Batch) error {
	storeBatch := txi.store.NewBatch()
	defer storeBatch.Close()

	for _, result := range b.Ops {
		hash := types.Tx(result.Tx).Hash()

		// index tx by events
		err := txi.indexEvents(result, hash, storeBatch)
		if err != nil {
			return err
		}

		// index by height (always)
		err = storeBatch.Set(keyForHeight(result), hash)
		if err != nil {
			return err
		}

		rawBytes, err := proto.Marshal(result)
		if err != nil {
			return err
		}
		// index by hash (always)
		err = storeBatch.Set(hash, rawBytes)
		if err != nil {
			return err
		}
	}

	return storeBatch.WriteSync()
}

// Index indexes a single transaction using the given list of events. Each key
// that indexed from the tx's events is a composite of the event type and the
// respective attribute's key delimited by a "." (eg. "account.number").
// Any event with an empty type is not indexed.
//
// If a transaction is indexed with the same hash as a previous transaction, it will
// be overwritten unless the tx result was NOT OK and the prior result was OK i.e.
// more transactions that successfully executed overwrite transactions that failed
// or successful yet older transactions.
func (txi *TxIndex) Index(result *abci.TxResult) error {
	b := txi.store.NewBatch()
	defer b.Close()

	hash := types.Tx(result.Tx).Hash()

	if !result.Result.IsOK() {
		oldResult, err := txi.Get(hash)
		if err != nil {
			return err
		}

		// if the new transaction failed and it's already indexed in an older block and was successful
		// we skip it as we want users to get the older successful transaction when they query.
		if oldResult != nil && oldResult.Result.Code == abci.CodeTypeOK {
			return nil
		}
	}

	// index tx by events
	err := txi.indexEvents(result, hash, b)
	if err != nil {
		return err
	}

	// index by height (always)
	err = b.Set(keyForHeight(result), hash)
	if err != nil {
		return err
	}

	rawBytes, err := proto.Marshal(result)
	if err != nil {
		return err
	}
	// index by hash (always)
	err = b.Set(hash, rawBytes)
	if err != nil {
		return err
	}

	return b.WriteSync()
}

func (txi *TxIndex) indexEvents(result *abci.TxResult, hash []byte, store dbm.Batch) error {
	for _, event := range result.Result.Events {
		txi.eventSeq = txi.eventSeq + 1
		// only index events with a non-empty type
		if len(event.Type) == 0 {
			continue
		}

		for _, attr := range event.Attributes {
			if len(attr.Key) == 0 {
				continue
			}

			// index if `index: true` is set
			compositeTag := fmt.Sprintf("%s.%s", event.Type, string(attr.Key))
			if attr.GetIndex() {
				err := store.Set(keyForEvent(compositeTag, attr.Value, result, txi.eventSeq), hash)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// Search performs a search using the given query.
//
// It breaks the query into conditions (like "tx.height > 5"). For each
// condition, it queries the DB index. One special use cases here: (1) if
// "tx.hash" is found, it returns tx result for it (2) for range queries it is
// better for the client to provide both lower and upper bounds, so we are not
// performing a full scan. Results from querying indexes are then intersected
// and returned to the caller, in no particular order.
//
// Search will exit early and return any result fetched so far,
// when a message is received on the context chan.
func (txi *TxIndex) Search(ctx context.Context, q *query.Query) ([]*abci.TxResult, error) {
	select {
	case <-ctx.Done():
		return make([]*abci.TxResult, 0), nil

	default:
	}

	var hashesInitialized bool
	filteredHashes := make(map[string][]byte)

	// get a list of conditions (like "tx.height > 5")
	conditions, err := q.Conditions()
	if err != nil {
		return nil, fmt.Errorf("error during parsing conditions from query: %w", err)
	}

	// if there is a hash condition, return the result immediately
	hash, ok, err := lookForHash(conditions)
	if err != nil {
		return nil, fmt.Errorf("error during searching for a hash in the query: %w", err)
	} else if ok {
		res, err := txi.Get(hash)
		switch {
		case err != nil:
			return []*abci.TxResult{}, fmt.Errorf("error while retrieving the result: %w", err)
		case res == nil:
			return []*abci.TxResult{}, nil
		default:
			return []*abci.TxResult{res}, nil
		}
	}

	var matchEvents bool
	var matchEventIdx int

	// If the match.events keyword is at the beginning of the query, we will only
	// return heights where the conditions are true within the same event
	// and set the matchEvents to true
	conditions, matchEvents = dedupMatchEvents(conditions)
	// conditions to skip because they're handled before "everything else"
	skipIndexes := make([]int, 0)

	if matchEvents {
		matchEventIdx = 0
		skipIndexes = append(skipIndexes, matchEventIdx)
	}

	// if there is a height condition ("tx.height=3"), extract it
	// var height int64
	// var heightIdx int
	var heightInfo HeightInfo
	if matchEvents {
		// If we are not matching events and tx.height = 3 occurs more than once, the later value will
		// overwrite the first one. For match.events it will create problems.
		conditions, heightInfo = dedupHeight(conditions)
	} else {
		heightInfo.height, heightInfo.heightEqIdx = lookForHeight(conditions)
	}
	if matchEvents && !heightInfo.onlyHeightEq {
		skipIndexes = append(skipIndexes, heightInfo.heightEqIdx)
	}
	// extract ranges
	// if both upper and lower bounds exist, it's better to get them in order not
	// no iterate over kvs that are not within range.
	//If we have a query range over height and want to still look for
	// specific event values we do not want to simply return all
	// transactios in this height range. We remember the height range info
	// and pass it on to match() to take into account when processing events.
	ranges, rangeIndexes, heightRange := indexer.LookForRangesWithHeight(conditions)
	heightInfo.heightRange = heightRange

	if len(ranges) > 0 {
		skipIndexes = append(skipIndexes, rangeIndexes...)

		for _, qr := range ranges {

			// If we have additional constraints and want to query per event
			// attributes, we cannot simply return all blocks for a height.
			// But we remember the height we want to find and forward it to
			// match(). If we only have the height constraint and match.events keyword
			// in the query (the second part of the ||), we don't need to query
			// per event conditions and return all events within the height range.
			if qr.Key == types.TxHeightKey && matchEvents && !heightInfo.onlyHeightRange {
				continue
			}
			if !hashesInitialized {
				filteredHashes = txi.matchRange(ctx, qr, startKey(qr.Key), filteredHashes, true, matchEvents, heightInfo)
				hashesInitialized = true

				// Ignore any remaining conditions if the first condition resulted
				// in no matches (assuming implicit AND operand).
				if len(filteredHashes) == 0 {
					break
				}
			} else {
				filteredHashes = txi.matchRange(ctx, qr, startKey(qr.Key), filteredHashes, false, matchEvents, heightInfo)
			}
		}
	}

	// for all other conditions
	for i, c := range conditions {
		if intInSlice(i, skipIndexes) {
			continue
		}

		if !hashesInitialized {
			filteredHashes = txi.match(ctx, c, startKeyForCondition(c, heightInfo.height), filteredHashes, true, matchEvents, heightInfo)
			hashesInitialized = true

			// Ignore any remaining conditions if the first condition resulted
			// in no matches (assuming implicit AND operand).
			if len(filteredHashes) == 0 {
				break
			}
		} else {
			filteredHashes = txi.match(ctx, c, startKeyForCondition(c, heightInfo.height), filteredHashes, false, matchEvents, heightInfo)
		}
	}

	results := make([]*abci.TxResult, 0, len(filteredHashes))
	resultMap := make(map[string]struct{})
	for _, h := range filteredHashes {

		hashString := string(h)
		if _, ok := resultMap[hashString]; !ok {
			resultMap[hashString] = struct{}{}
			res, err := txi.Get(h)
			if err != nil {
				return nil, fmt.Errorf("failed to get Tx{%X}: %w", h, err)
			}
			results = append(results, res)
		}
		// Potentially exit early.
		select {
		case <-ctx.Done():
			break
		default:
		}
	}

	return results, nil
}

func lookForHash(conditions []query.Condition) (hash []byte, ok bool, err error) {
	for _, c := range conditions {
		if c.CompositeKey == types.TxHashKey {
			decoded, err := hex.DecodeString(c.Operand.(string))
			return decoded, true, err
		}
	}
	return
}

// lookForHeight returns a height if there is an "height=X" condition.
func lookForHeight(conditions []query.Condition) (height int64, heightIdx int) {
	for i, c := range conditions {
		if c.CompositeKey == types.TxHeightKey && c.Op == query.OpEqual {
			return c.Operand.(*big.Int).Int64(), i
		}
	}
	return 0, -1
}
func (txi *TxIndex) setTmpHashes(tmpHeights map[string][]byte, it dbm.Iterator, matchEvents bool) {
	if matchEvents {
		eventSeq := extractEventSeqFromKey(it.Key())
		tmpHeights[string(it.Value())+eventSeq] = it.Value()
	} else {
		tmpHeights[string(it.Value())] = it.Value()
	}
}

// match returns all matching txs by hash that meet a given condition and start
// key. An already filtered result (filteredHashes) is provided such that any
// non-intersecting matches are removed.
//
// NOTE: filteredHashes may be empty if no previous condition has matched.
func (txi *TxIndex) match(
	ctx context.Context,
	c query.Condition,
	startKeyBz []byte,
	filteredHashes map[string][]byte,
	firstRun bool,
	matchEvents bool,
	heightInfo HeightInfo,
) map[string][]byte {
	// A previous match was attempted but resulted in no matches, so we return
	// no matches (assuming AND operand).
	if !firstRun && len(filteredHashes) == 0 {
		return filteredHashes
	}

	tmpHashes := make(map[string][]byte)

	switch {
	case c.Op == query.OpEqual:
		it, err := dbm.IteratePrefix(txi.store, startKeyBz)
		if err != nil {
			panic(err)
		}
		defer it.Close()

		for ; it.Valid(); it.Next() {

			// If we have a height range in a query, we need only transactions
			// for this height

			if matchEvents {
				keyHeight, err := extractHeightFromKey(it.Key())
				if err != nil || !checkHeightConditions(heightInfo, keyHeight) {
					continue
				}

			}

			txi.setTmpHashes(tmpHashes, it, matchEvents)
			// Potentially exit early.
			select {
			case <-ctx.Done():
				break
			default:
			}
		}
		if err := it.Error(); err != nil {
			panic(err)
		}

	case c.Op == query.OpExists:
		// XXX: can't use startKeyBz here because c.Operand is nil
		// (e.g. "account.owner/<nil>/" won't match w/ a single row)
		it, err := dbm.IteratePrefix(txi.store, startKey(c.CompositeKey))
		if err != nil {
			panic(err)
		}
		defer it.Close()

		for ; it.Valid(); it.Next() {
			if matchEvents {
				keyHeight, err := extractHeightFromKey(it.Key())
				if err != nil || !checkHeightConditions(heightInfo, keyHeight) {
					continue
				}

			}
			txi.setTmpHashes(tmpHashes, it, matchEvents)

			// Potentially exit early.
			select {
			case <-ctx.Done():
				break
			default:
			}
		}
		if err := it.Error(); err != nil {
			panic(err)
		}

	case c.Op == query.OpContains:
		// XXX: startKey does not apply here.
		// For example, if startKey = "account.owner/an/" and search query = "account.owner CONTAINS an"
		// we can't iterate with prefix "account.owner/an/" because we might miss keys like "account.owner/Ulan/"
		it, err := dbm.IteratePrefix(txi.store, startKey(c.CompositeKey))
		if err != nil {
			panic(err)
		}
		defer it.Close()

		for ; it.Valid(); it.Next() {
			if !isTagKey(it.Key()) {
				continue
			}

			if strings.Contains(extractValueFromKey(it.Key()), c.Operand.(string)) {
				if matchEvents {
					keyHeight, err := extractHeightFromKey(it.Key())
					if err != nil || !checkHeightConditions(heightInfo, keyHeight) {
						continue
					}
				}
				txi.setTmpHashes(tmpHashes, it, matchEvents)
			}

			// Potentially exit early.
			select {
			case <-ctx.Done():
				break
			default:
			}
		}
		if err := it.Error(); err != nil {
			panic(err)
		}
	default:
		panic("other operators should be handled already")
	}

	if len(tmpHashes) == 0 || firstRun {
		// Either:
		//
		// 1. Regardless if a previous match was attempted, which may have had
		// results, but no match was found for the current condition, then we
		// return no matches (assuming AND operand).
		//
		// 2. A previous match was not attempted, so we return all results.
		return tmpHashes
	}

	// Remove/reduce matches in filteredHashes that were not found in this
	// match (tmpHashes).
	for k, v := range filteredHashes {
		tmpHash := tmpHashes[k]
		if tmpHash == nil || !bytes.Equal(tmpHash, v) {
			delete(filteredHashes, k)

			// Potentially exit early.
			select {
			case <-ctx.Done():
				break
			default:
			}
		}
	}

	return filteredHashes
}

const blockToSearch = 5000

// matchRange returns all matching txs by hash that meet a given queryRange and
// start key. An already filtered result (filteredHashes) is provided such that
// any non-intersecting matches are removed.
//
// NOTE: filteredHashes may be empty if no previous condition has matched.
func (txi *TxIndex) matchRange(
	ctx context.Context,
	qr indexer.QueryRange,
	startKey []byte,
	filteredHashes map[string][]byte,
	firstRun bool,
	matchEvents bool,
	heightInfo HeightInfo,
) map[string][]byte {
	// A previous match was attempted but resulted in no matches, so we return
	// no matches (assuming AND operand).
	if !firstRun && len(filteredHashes) == 0 {
		return filteredHashes
	}

	lowerBound, lowerOk := qr.LowerBoundValue().(*big.Int)
	upperBound, upperOk := qr.UpperBoundValue().(*big.Int)
	if !lowerOk && !upperOk {
		return filteredHashes
	}
	// include =
	rangeBound := big.NewInt(blockToSearch - 1)
	if lowerBound == nil {
		lowerBound = new(big.Int).Sub(upperBound, rangeBound)
	} else if upperBound == nil {
		upperBound = new(big.Int).Add(lowerBound, rangeBound)
	}

	lowerHeight := lowerBound.Int64()
	upperHeight := upperBound.Int64()

	// when search with upperHeight < blockToSearch
	if lowerHeight < 1 {
		lowerHeight = 1
	}

	// upper >= lower and upperHeight-lowerHeight <= blockToSearch
	if lowerHeight > upperHeight || upperHeight-lowerHeight > blockToSearch {
		return filteredHashes
	}

	tmpHashes := make(map[string][]byte)
	sizeBytes := len(startKey) + 4 + len(tagKeySeparator)
	fromKey := joinSizeBytes(sizeBytes, startKey, []byte{byte(lowerHeight >> 24), byte(lowerHeight >> 16), byte(lowerHeight >> 8), byte(lowerHeight)}, []byte(tagKeySeparator))
	toKey := joinSizeBytes(sizeBytes, startKey, []byte{byte(upperHeight >> 24), byte(upperHeight >> 16), byte(upperHeight >> 8), byte(upperHeight)}, []byte(tagKeySeparator))

	// already have correct range
	it, err := txi.store.Iterator(fromKey, toKey)
	if err != nil {
		panic(err)
	}
	defer it.Close()

LOOP:
	for ; it.Valid(); it.Next() {

		if !isTagKey(it.Key()) {
			continue
		}

		if matchEvents && qr.Key != types.TxHeightKey {
			keyHeight, err := extractHeightFromKey(it.Key())
			if err != nil || !checkHeightConditions(heightInfo, keyHeight) {
				continue LOOP
			}
		}

		txi.setTmpHashes(tmpHashes, it, matchEvents)

		// XXX: passing time in a ABCI Events is not yet implemented
		// case time.Time:
		// 	v := strconv.ParseInt(extractValueFromKey(it.Key()), 10, 64)
		// 	if v == r.upperBound {
		// 		break
		// 	}

		// Potentially exit early.
		select {
		case <-ctx.Done():
			break
		default:
		}
	}
	if err := it.Error(); err != nil {
		panic(err)
	}

	if len(tmpHashes) == 0 || firstRun {
		// Either:
		//
		// 1. Regardless if a previous match was attempted, which may have had
		// results, but no match was found for the current condition, then we
		// return no matches (assuming AND operand).
		//
		// 2. A previous match was not attempted, so we return all results.
		return tmpHashes
	}

	// Remove/reduce matches in filteredHashes that were not found in this
	// match (tmpHashes).
	for k, v := range filteredHashes {
		tmpHash := tmpHashes[k]
		if tmpHash == nil || !bytes.Equal(tmpHashes[k], v) {
			delete(filteredHashes, k)

			// Potentially exit early.
			select {
			case <-ctx.Done():
				break
			default:
			}
		}
	}

	return filteredHashes
}

// Keys

func isTagKey(key []byte) bool {
	// Normally, if the event was indexed with an event sequence, the number of
	// tags should 4. Alternatively it should be 3 if the event was not indexed
	// with the corresponding event sequence. However, some attribute values in
	// production can contain the tag separator. Therefore, the condition is >= 3.
	numTags := strings.Count(string(key), tagKeySeparator)
	return numTags >= 3
}

func extractHeightFromKey(key []byte) (int64, error) {
	parts := strings.SplitN(string(key), tagKeySeparator, -1)
	return strconv.ParseInt(parts[len(parts)-2], 10, 64)
}
func extractValueFromKey(key []byte) string {
	keyString := string(key)
	parts := strings.SplitN(keyString, tagKeySeparator, -1)
	partsLen := len(parts)
	value := strings.TrimPrefix(keyString, parts[0]+tagKeySeparator)

	suffix := ""
	suffixLen := 2

	for i := 1; i <= suffixLen; i++ {
		suffix = tagKeySeparator + parts[partsLen-i] + suffix
	}
	return strings.TrimSuffix(value, suffix)
}

func extractEventSeqFromKey(key []byte) string {
	parts := strings.SplitN(string(key), tagKeySeparator, -1)

	lastEl := parts[len(parts)-1]

	if strings.Contains(lastEl, eventSeqSeparator) {
		return strings.SplitN(lastEl, eventSeqSeparator, 2)[1]
	}
	return "0"
}
func keyForEvent(key string, value []byte, result *abci.TxResult, eventSeq int64) []byte {
	return []byte(fmt.Sprintf("%s/%s/%d/%d%s",
		key,
		value,
		result.Height,
		result.Index,
		eventSeqSeparator+strconv.FormatInt(eventSeq, 10),
	))
}

func joinBytes(s ...[]byte) []byte {
	n := 0
	for _, v := range s {
		n += len(v)
	}

	return joinSizeBytes(n, s...)
}

func joinSizeBytes(n int, s ...[]byte) []byte {
	b, i := make([]byte, n), 0
	for _, v := range s {
		i += copy(b[i:], v)
	}
	return b
}

func keyForHeight(result *abci.TxResult) []byte {
	keyBytes := joinBytes([]byte(types.TxHeightKey), []byte(tagKeySeparator),
		[]byte{byte(result.Height >> 24), byte(result.Height >> 16), byte(result.Height >> 8), byte(result.Height)},
		[]byte(fmt.Sprintf("/%d/%d%s",
			result.Height,
			result.Index,
			// Added to facilitate having the eventSeq in event keys
			// Otherwise queries break expecting 5 entries
			eventSeqSeparator+"0",
		)),
	)
	return keyBytes
}

func startKeyForCondition(c query.Condition, height int64) []byte {
	if height > 0 {
		return startKey(c.CompositeKey, c.Operand, height)
	}
	return startKey(c.CompositeKey, c.Operand)
}

func startKey(fields ...interface{}) []byte {
	var b bytes.Buffer
	for _, f := range fields {
		b.Write([]byte(fmt.Sprintf("%v", f) + tagKeySeparator))
	}
	return b.Bytes()
}

func checkBounds(ranges indexer.QueryRange, v *big.Int) bool {
	include := true
	lowerBound := ranges.LowerBoundValue()
	upperBound := ranges.UpperBoundValue()
	if lowerBound != nil && v.Cmp(lowerBound.(*big.Int)) == -1 {
		include = false
	}

	if upperBound != nil && v.Cmp(upperBound.(*big.Int)) == 1 {
		include = false
	}

	return include
}
