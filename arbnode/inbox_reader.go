// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package arbnode

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/log"
	flag "github.com/spf13/pflag"

	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/headerreader"
	"github.com/offchainlabs/nitro/util/stopwaiter"
)

type InboxReaderConfig struct {
	DelayBlocks     uint64        `koanf:"delay-blocks"`
	CheckDelay      time.Duration `koanf:"check-delay"`
	HardReorg       bool          `koanf:"hard-reorg"`
	MinBlocksToRead uint64        `koanf:"min-blocks-to-read"`
}

func InboxReaderConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Uint64(prefix+".delay-blocks", DefaultInboxReaderConfig.DelayBlocks, "number of latest blocks to ignore to reduce reorgs")
	f.Duration(prefix+".check-delay", DefaultInboxReaderConfig.CheckDelay, "the maximum time to wait between inbox checks (if not enough new blocks are found)")
	f.Bool(prefix+".hard-reorg", DefaultInboxReaderConfig.HardReorg, "erase future transactions in addition to overwriting existing ones on reorg")
	f.Uint64(prefix+".min-blocks-to-read", DefaultInboxReaderConfig.MinBlocksToRead, "the minimum number of blocks to read at once (when caught up lowers load on L1)")
}

var DefaultInboxReaderConfig = InboxReaderConfig{
	DelayBlocks:     0,
	CheckDelay:      time.Minute,
	HardReorg:       false,
	MinBlocksToRead: 1,
}

var TestInboxReaderConfig = InboxReaderConfig{
	DelayBlocks:     0,
	CheckDelay:      time.Millisecond * 10,
	HardReorg:       false,
	MinBlocksToRead: 1,
}

type InboxReader struct {
	stopwaiter.StopWaiter

	// Only in run thread
	caughtUp          bool
	firstMessageBlock *big.Int
	config            *InboxReaderConfig

	// Thread safe
	tracker        *InboxTracker
	delayedBridge  *DelayedBridge
	sequencerInbox *SequencerInbox
	caughtUpChan   chan bool
	client         arbutil.L1Interface
	l1Reader       *headerreader.HeaderReader

	// Atomic
	lastSeenBatchCount uint64

	// Behind the mutex
	lastReadMutex      sync.RWMutex
	lastReadBlock      uint64
	lastReadBatchCount uint64
}

func NewInboxReader(tracker *InboxTracker, client arbutil.L1Interface, l1Reader *headerreader.HeaderReader, firstMessageBlock *big.Int, delayedBridge *DelayedBridge, sequencerInbox *SequencerInbox, config *InboxReaderConfig) (*InboxReader, error) {
	return &InboxReader{
		tracker:           tracker,
		delayedBridge:     delayedBridge,
		sequencerInbox:    sequencerInbox,
		client:            client,
		l1Reader:          l1Reader,
		firstMessageBlock: firstMessageBlock,
		caughtUpChan:      make(chan bool, 1),
		config:            config,
	}, nil
}

func (r *InboxReader) Start(ctxIn context.Context) error {
	r.StopWaiter.Start(ctxIn)
	r.CallIteratively(func(ctx context.Context) time.Duration {
		err := r.run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "header not found") {
			log.Warn("error reading inbox", "err", err)
		}
		return time.Second
	})

	// Ensure we read the init message before other things start up
	for i := 0; ; i++ {
		batchCount, err := r.tracker.GetBatchCount()
		if err != nil {
			return err
		}
		if batchCount > 0 {
			// Validate the init message matches our L2 blockchain
			message, err := r.tracker.GetDelayedMessage(0)
			if err != nil {
				return err
			}
			initChainId, err := message.ParseInitMessage()
			if err != nil {
				return err
			}
			configChainId := r.tracker.txStreamer.bc.Config().ChainID
			if initChainId.Cmp(configChainId) != 0 {
				return fmt.Errorf("expected L2 chain ID %v but read L2 chain ID %v from init message in L1 inbox", configChainId, initChainId)
			}
			break
		}
		if i == 30*10 {
			return errors.New("failed to read init message")
		}
		time.Sleep(time.Millisecond * 100)
	}

	return nil
}

func (r *InboxReader) Tracker() *InboxTracker {
	return r.tracker
}

func (r *InboxReader) DelayedBridge() *DelayedBridge {
	return r.delayedBridge
}

func (ir *InboxReader) run(ctx context.Context) error {
	from, err := ir.getNextBlockToRead()
	if err != nil {
		return err
	}
	newHeaders, unsubscribe := ir.l1Reader.Subscribe(false)
	defer unsubscribe()
	blocksToFetch := uint64(100)
	neededBlockAdvance := ir.config.DelayBlocks + arbmath.SaturatingUSub(ir.config.MinBlocksToRead, 1)
	seenBatchCount := uint64(0)
	seenBatchCountStored := uint64(math.MaxUint64)
	storeSeenBatchCount := func() {
		if seenBatchCountStored != seenBatchCount {
			atomic.StoreUint64(&ir.lastSeenBatchCount, seenBatchCount)
			seenBatchCountStored = seenBatchCount
		}
	}
	defer storeSeenBatchCount() // in case of error
	for {

		latestHeader, err := ir.l1Reader.LastHeader(ctx)
		if err != nil {
			return err
		}
		currentHeight := latestHeader.Number

		neededBlockHeight := arbmath.BigAddByUint(from, neededBlockAdvance)
		checkDelayTimer := time.NewTimer(ir.config.CheckDelay)
	WaitForHeight:
		for arbmath.BigLessThan(currentHeight, neededBlockHeight) {
			select {
			case latestHeader = <-newHeaders:
				if latestHeader == nil {
					// shutting down
					return nil
				}
				currentHeight = new(big.Int).Set(latestHeader.Number)
			case <-ctx.Done():
				return nil
			case <-checkDelayTimer.C:
				break WaitForHeight
			}
		}
		checkDelayTimer.Stop()

		if ir.config.DelayBlocks > 0 {
			currentHeight = new(big.Int).Sub(currentHeight, new(big.Int).SetUint64(ir.config.DelayBlocks))
			if currentHeight.Cmp(ir.firstMessageBlock) < 0 {
				currentHeight = new(big.Int).Set(ir.firstMessageBlock)
			}
		}

		reorgingDelayed := false
		reorgingSequencer := false
		missingDelayed := false
		missingSequencer := false

		{
			checkingDelayedCount, err := ir.delayedBridge.GetMessageCount(ctx, currentHeight)
			if err != nil {
				return err
			}
			ourLatestDelayedCount, err := ir.tracker.GetDelayedCount()
			if err != nil {
				return err
			}
			if ourLatestDelayedCount < checkingDelayedCount {
				checkingDelayedCount = ourLatestDelayedCount
				missingDelayed = true
			} else if ourLatestDelayedCount > checkingDelayedCount && ir.config.HardReorg {
				log.Info("backwards reorg of delayed messages", "from", ourLatestDelayedCount, "to", checkingDelayedCount)
				err = ir.tracker.ReorgDelayedTo(checkingDelayedCount)
				if err != nil {
					return err
				}
			}
			if checkingDelayedCount > 0 {
				checkingDelayedSeqNum := checkingDelayedCount - 1
				l1DelayedAcc, err := ir.delayedBridge.GetAccumulator(ctx, checkingDelayedSeqNum, currentHeight)
				if err != nil {
					return err
				}
				dbDelayedAcc, err := ir.tracker.GetDelayedAcc(checkingDelayedSeqNum)
				if err != nil {
					return err
				}
				if dbDelayedAcc != l1DelayedAcc {
					reorgingDelayed = true
				}
			}
		}

		seenBatchCount, err = ir.sequencerInbox.GetBatchCount(ctx, currentHeight)
		if err != nil {
			seenBatchCount = 0
			return err
		}
		checkingBatchCount := seenBatchCount
		{
			ourLatestBatchCount, err := ir.tracker.GetBatchCount()
			if err != nil {
				return err
			}
			if ourLatestBatchCount < checkingBatchCount {
				checkingBatchCount = ourLatestBatchCount
				missingSequencer = true
			} else if ourLatestBatchCount > checkingBatchCount && ir.config.HardReorg {
				err = ir.tracker.ReorgBatchesTo(checkingBatchCount)
				if err != nil {
					return err
				}
			}
			if checkingBatchCount > 0 {
				checkingBatchSeqNum := checkingBatchCount - 1
				l1BatchAcc, err := ir.sequencerInbox.GetAccumulator(ctx, checkingBatchSeqNum, currentHeight)
				if err != nil {
					return err
				}
				dbBatchAcc, err := ir.tracker.GetBatchAcc(checkingBatchSeqNum)
				if err != nil {
					return err
				}
				if dbBatchAcc != l1BatchAcc {
					reorgingSequencer = true
				}
			}
		}

		if !missingDelayed && !reorgingDelayed && !missingSequencer && !reorgingSequencer {
			// There's nothing to do
			from = arbmath.BigAddByUint(currentHeight, 1)
			ir.lastReadMutex.Lock()
			ir.lastReadBlock = currentHeight.Uint64()
			ir.lastReadBatchCount = checkingBatchCount
			ir.lastReadMutex.Unlock()
			storeSeenBatchCount()
			continue
		}

		readAnyBatches := false
		for {
			if ctx.Err() != nil {
				// the context is done, shut down
				// nolint:nilerr
				return nil
			}
			if from.Cmp(currentHeight) > 0 {
				if missingDelayed {
					reorgingDelayed = true
				}
				if missingSequencer {
					reorgingSequencer = true
				}
				if !reorgingDelayed && !reorgingSequencer {
					break
				} else {
					from = currentHeight
				}
			}
			to := new(big.Int).Add(from, new(big.Int).SetUint64(blocksToFetch))
			if to.Cmp(currentHeight) > 0 {
				to = currentHeight
			}
			var delayedMessages []*DelayedInboxMessage
			delayedMessages, err := ir.delayedBridge.LookupMessagesInRange(ctx, from, to)
			if err != nil {
				return err
			}
			sequencerBatches, err := ir.sequencerInbox.LookupBatchesInRange(ctx, from, to)
			if err != nil {
				return err
			}
			if !ir.caughtUp && to.Cmp(currentHeight) == 0 {
				// TODO better caught up tracking
				ir.caughtUp = true
				ir.caughtUpChan <- true
			}
			if len(sequencerBatches) > 0 {
				missingSequencer = false
				reorgingSequencer = false
				firstBatch := sequencerBatches[0]
				if firstBatch.SequenceNumber > 0 {
					haveAcc, err := ir.tracker.GetBatchAcc(firstBatch.SequenceNumber - 1)
					if errors.Is(err, accumulatorNotFound) {
						reorgingSequencer = true
					} else if err != nil {
						return err
					} else if haveAcc != firstBatch.BeforeInboxAcc {
						reorgingSequencer = true
					}
				}
				if !reorgingSequencer {
					// Skip any batches we already have in the database
					for len(sequencerBatches) > 0 {
						batch := sequencerBatches[0]
						haveAcc, err := ir.tracker.GetBatchAcc(batch.SequenceNumber)
						if errors.Is(err, accumulatorNotFound) {
							// This batch is new
							break
						} else if err != nil {
							// Unknown error (database error?)
							return err
						} else if haveAcc == batch.BeforeInboxAcc {
							// Skip this batch, as we already have it in the database
							sequencerBatches = sequencerBatches[1:]
						} else {
							// The first batch BeforeInboxAcc matches, but this batch doesn't,
							// so we'll successfully reorg it when we hit the addMessages
							break
						}
					}
				}
			} else if missingSequencer && to.Cmp(currentHeight) >= 0 {
				// We were missing sequencer batches but didn't find any.
				// This must mean that the sequencer batches are in the past.
				reorgingSequencer = true
			}

			if len(delayedMessages) > 0 {
				missingDelayed = false
				reorgingDelayed = false
				firstMsg := delayedMessages[0]
				beforeAcc := firstMsg.BeforeInboxAcc
				beforeCount, err := firstMsg.Message.Header.SeqNum()
				if err != nil {
					return err
				}
				if beforeCount > 0 {
					haveAcc, err := ir.tracker.GetDelayedAcc(beforeCount - 1)
					if errors.Is(err, accumulatorNotFound) {
						reorgingDelayed = true
					} else if err != nil {
						return err
					} else if haveAcc != beforeAcc {
						reorgingDelayed = true
					}
				}
			} else if missingDelayed && to.Cmp(currentHeight) >= 0 {
				// We were missing delayed messages but didn't find any.
				// This must mean that the delayed messages are in the past.
				reorgingDelayed = true
			}

			log.Trace("looking up messages", "from", from.String(), "to", to.String(), "reorgingDelayed", reorgingDelayed, "reorgingSequencer", reorgingSequencer)
			if !reorgingDelayed && !reorgingSequencer && (len(delayedMessages) != 0 || len(sequencerBatches) != 0) {
				delayedMismatch, err := ir.addMessages(ctx, sequencerBatches, delayedMessages)
				if err != nil {
					return err
				}
				if delayedMismatch {
					reorgingDelayed = true
				}
				if len(sequencerBatches) > 0 {
					readAnyBatches = true
					ir.lastReadMutex.Lock()
					ir.lastReadBlock = to.Uint64()
					ir.lastReadBatchCount = sequencerBatches[len(sequencerBatches)-1].SequenceNumber + 1
					ir.lastReadMutex.Unlock()
					storeSeenBatchCount()
				}
			}
			if reorgingDelayed || reorgingSequencer {
				from, err = ir.getPrevBlockForReorg(from)
				if err != nil {
					return err
				}
			} else {
				from = from.Add(to, big.NewInt(1))
			}
		}

		if !readAnyBatches {
			ir.lastReadMutex.Lock()
			ir.lastReadBlock = currentHeight.Uint64()
			ir.lastReadBatchCount = checkingBatchCount
			ir.lastReadMutex.Unlock()
			storeSeenBatchCount()
		}
	}
}

func (r *InboxReader) addMessages(ctx context.Context, sequencerBatches []*SequencerInboxBatch, delayedMessages []*DelayedInboxMessage) (bool, error) {
	err := r.tracker.AddDelayedMessages(delayedMessages)
	if err != nil {
		return false, err
	}
	err = r.tracker.AddSequencerBatches(ctx, r.client, sequencerBatches)
	if errors.Is(err, delayedMessagesMismatch) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	return false, nil
}

func (r *InboxReader) getPrevBlockForReorg(from *big.Int) (*big.Int, error) {
	if from.Cmp(r.firstMessageBlock) <= 0 {
		return nil, errors.New("can't get older messages")
	}
	newFrom := new(big.Int).Sub(from, big.NewInt(10))
	if newFrom.Cmp(r.firstMessageBlock) < 0 {
		newFrom = new(big.Int).Set(r.firstMessageBlock)
	}
	return newFrom, nil
}

func (r *InboxReader) getNextBlockToRead() (*big.Int, error) {
	delayedCount, err := r.tracker.GetDelayedCount()
	if err != nil {
		return nil, err
	}
	if delayedCount == 0 {
		return new(big.Int).Set(r.firstMessageBlock), nil
	}
	msg, err := r.tracker.GetDelayedMessage(delayedCount - 1)
	if err != nil {
		return nil, err
	}
	msgBlock := new(big.Int).SetUint64(msg.Header.BlockNumber)
	if arbmath.BigLessThan(msgBlock, r.firstMessageBlock) {
		msgBlock.Set(r.firstMessageBlock)
	}
	return msgBlock, nil
}

func (r *InboxReader) GetSequencerMessageBytes(ctx context.Context, seqNum uint64) ([]byte, error) {
	metadata, err := r.tracker.GetBatchMetadata(seqNum)
	if err != nil {
		return nil, err
	}
	blockNum := big.NewInt(0).SetUint64(metadata.L1Block)
	seqBatches, err := r.sequencerInbox.LookupBatchesInRange(ctx, blockNum, blockNum)
	if err != nil {
		return nil, err
	}
	for _, batch := range seqBatches {
		if batch.SequenceNumber == seqNum {
			return batch.Serialize(ctx, r.client)
		}
	}
	return nil, errors.New("sequencer batch not found")
}

func (r *InboxReader) GetLastReadBlockAndBatchCount() (uint64, uint64) {
	r.lastReadMutex.RLock()
	defer r.lastReadMutex.RUnlock()
	return r.lastReadBlock, r.lastReadBatchCount
}

// >0 - last batchcount seen in run() - only written after lastReadBatchCount updated
// 0 - no batch seen, error
func (r *InboxReader) GetLastSeenBatchCount() uint64 {
	return atomic.LoadUint64(&r.lastSeenBatchCount)
}

func (r *InboxReader) GetDelayBlocks() uint64 {
	return r.config.DelayBlocks
}
