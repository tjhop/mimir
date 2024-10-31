// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/storage/tsdb/bucketindex/updater.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package bucketindex

import (
	"context"
	"encoding/json"
	"io"
	"path"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/concurrency"
	"github.com/grafana/dskit/runutil"
	"github.com/oklog/ulid"
	"github.com/pkg/errors"
	"github.com/thanos-io/objstore"

	"github.com/grafana/mimir/pkg/storage/bucket"
	"github.com/grafana/mimir/pkg/storage/tsdb/block"
)

var (
	ErrBlockMetaNotFound          = block.ErrorSyncMetaNotFound
	ErrBlockMetaCorrupted         = block.ErrorSyncMetaCorrupted
	ErrBlockDeletionMarkNotFound  = errors.New("block deletion mark not found")
	ErrBlockDeletionMarkCorrupted = errors.New("block deletion mark corrupted")
)

// Updater is responsible to generate an update in-memory bucket index.
type Updater struct {
	bkt    objstore.InstrumentedBucket
	logger log.Logger
}

func NewUpdater(bkt objstore.Bucket, userID string, cfgProvider bucket.TenantConfigProvider, logger log.Logger) *Updater {
	return &Updater{
		bkt:    bucket.NewUserBucketClient(userID, bkt, cfgProvider),
		logger: logger,
	}
}

// UpdateIndex generates the bucket index and returns it, without storing it to the storage.
// If the old index is not passed in input, then the bucket index will be generated from scratch.
func (w *Updater) UpdateIndex(ctx context.Context, old *Index) (*Index, map[ulid.ULID]error, error) {
	var oldBlocks []*Block
	var oldBlockDeletionMarks []*BlockDeletionMark

	// Use the old index if provided, and it is using the latest version format.
	if old != nil && old.Version == IndexVersion2 {
		oldBlocks = old.Blocks
		oldBlockDeletionMarks = old.BlockDeletionMarks
	}

	// It's important to list and update deletion marks *before* we list the blocks in the bucket in
	// order to avoid a race condition in case there are 2 processes updating the bucket index at the same time.
	//
	// Updating markers before the blocks guarantees that, if another process is deleting a block while
	// we do the markers or blocks listing and updating, we end up in one of the following situations (which are
	// all legit):
	// 1. Both the block and deletion mark don't exist anymore
	// 2. The marker exists but the block doesn't because it has been deleted between listing markers
	//    and listing blocks
	// 3. Both the block and the deletion mark still exist
	//
	// But it can't happen that we end up in a situation where the deletion mark doesn't exist and
	// the block still exists, which is what we want to avoid, otherwise we may update the bucket
	// index with a block that has been deleted, it is still referenced in the list of blocks in the
	// index, but its deletion mark is not referenced anymore in the index.
	blockDeletionMarks, err := w.updateBlockDeletionMarks(ctx, oldBlockDeletionMarks)
	if err != nil {
		return nil, nil, err
	}

	blocks, partials, err := w.updateBlocks(ctx, oldBlocks)
	if err != nil {
		return nil, nil, err
	}

	return &Index{
		Version:            IndexVersion2,
		Blocks:             blocks,
		BlockDeletionMarks: blockDeletionMarks,
		UpdatedAt:          time.Now().Unix(),
	}, partials, nil
}

func (w *Updater) updateBlocks(ctx context.Context, old []*Block) (blocks []*Block, partials map[ulid.ULID]error, _ error) {
	discovered := map[ulid.ULID]struct{}{}
	partials = map[ulid.ULID]error{}

	// Find all blocks in the storage.
	err := w.bkt.Iter(ctx, "", func(name string) error {
		if id, ok := block.IsBlockDir(name); ok {
			discovered[id] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, nil, errors.Wrap(err, "list blocks")
	}

	// Since blocks are immutable, all blocks already existing in the index can just be copied.
	for _, b := range old {
		if _, ok := discovered[b.ID]; ok {
			blocks = append(blocks, b)
			delete(discovered, b.ID)
		}
	}

	level.Info(w.logger).Log("msg", "listed all blocks in storage", "newly_discovered", len(discovered), "existing", len(old))

	// Remaining blocks are new ones and we have to fetch the meta.json for each of them, in order
	// to find out if their upload has been completed (meta.json is uploaded last) and get the block
	// information to store in the bucket index.
	// Process concurrently, but limit the number of concurrent requests.
	discoveredSlice := make([]ulid.ULID, 0, len(discovered))
	for id := range discovered {
		discoveredSlice = append(discoveredSlice, id)
	}
	assignMu := sync.Mutex{}
	err = concurrency.ForEachJob(ctx, len(discovered), 4, func(ctx context.Context, idx int) error {
		id := discoveredSlice[idx]
		b, err := w.updateBlockIndexEntry(ctx, id)

		// Processing is done, the rest can be done sequentially.
		assignMu.Lock()
		defer assignMu.Unlock()

		if err == nil {
			blocks = append(blocks, b)
			return nil
		}

		if errors.Is(err, ErrBlockMetaNotFound) {
			partials[id] = err
			level.Warn(w.logger).Log("msg", "skipped partial block when updating bucket index", "block", id.String())
			return nil
		}
		if errors.Is(err, ErrBlockMetaCorrupted) {
			partials[id] = err
			level.Error(w.logger).Log("msg", "skipped block with corrupted meta.json when updating bucket index", "block", id.String(), "err", err)
			return nil
		}
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	level.Info(w.logger).Log("msg", "fetched blocks metas for newly discovered blocks", "total_blocks", len(blocks), "partial_errors", len(partials))

	return blocks, partials, nil
}

func (w *Updater) updateBlockIndexEntry(ctx context.Context, id ulid.ULID) (*Block, error) {
	// Set a generous timeout for fetching the meta.json and getting the attributes of the same file.
	// This protects against operations that can take unbounded time.
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	metaFile := path.Join(id.String(), block.MetaFilename)

	// Get the block's meta.json file.
	r, err := w.bkt.Get(ctx, metaFile)
	if w.bkt.IsObjNotFoundErr(err) {
		return nil, ErrBlockMetaNotFound
	}
	if err != nil {
		return nil, errors.Wrapf(err, "get block meta file: %v", metaFile)
	}
	defer runutil.CloseWithLogOnErr(w.logger, r, "close get block meta file")

	metaContent, err := io.ReadAll(r)
	if err != nil {
		return nil, errors.Wrapf(err, "read block meta file: %v", metaFile)
	}

	// Unmarshal it.
	m := block.Meta{}
	if err := json.Unmarshal(metaContent, &m); err != nil {
		return nil, errors.Wrapf(ErrBlockMetaCorrupted, "unmarshal block meta file %s: %v", metaFile, err)
	}

	if m.Version != block.TSDBVersion1 {
		return nil, errors.Errorf("unexpected block meta version: %s version: %d", metaFile, m.Version)
	}

	block := BlockFromThanosMeta(m)

	// Get the meta.json attributes.
	attrs, err := w.bkt.Attributes(ctx, metaFile)
	if err != nil {
		return nil, errors.Wrapf(err, "read meta file attributes: %v", metaFile)
	}

	// Since the meta.json file is the last file of a block being uploaded and it's immutable
	// we can safely assume that the last modified timestamp of the meta.json is the time when
	// the block has completed to be uploaded.
	block.UploadedAt = attrs.LastModified.Unix()

	return block, nil
}

func (w *Updater) updateBlockDeletionMarks(ctx context.Context, old []*BlockDeletionMark) ([]*BlockDeletionMark, error) {
	out := make([]*BlockDeletionMark, 0, len(old))

	// Find all markers in the storage.
	discovered, err := block.ListBlockDeletionMarks(ctx, w.bkt)
	if err != nil {
		return nil, err
	}

	level.Info(w.logger).Log("msg", "listed deletion markers", "count", len(discovered))

	// Since deletion marks are immutable, all markers already existing in the index can just be copied.
	for _, m := range old {
		if _, ok := discovered[m.ID]; ok {
			out = append(out, m)
			delete(discovered, m.ID)
		}
	}

	// Remaining markers are new ones and we have to fetch them.
	for id := range discovered {
		m, err := w.updateBlockDeletionMarkIndexEntry(ctx, id)
		if errors.Is(err, ErrBlockDeletionMarkNotFound) {
			// This could happen if the block is permanently deleted between the "list objects" and now.
			level.Warn(w.logger).Log("msg", "skipped missing block deletion mark when updating bucket index", "block", id.String())
			continue
		}
		if errors.Is(err, ErrBlockDeletionMarkCorrupted) {
			level.Error(w.logger).Log("msg", "skipped corrupted block deletion mark when updating bucket index", "block", id.String(), "err", err)
			continue
		}
		if err != nil {
			return nil, err
		}

		out = append(out, m)
	}

	level.Info(w.logger).Log("msg", "updated deletion markers for recently marked blocks", "count", len(discovered), "total_deletion_markers", len(out))

	return out, nil
}

func (w *Updater) updateBlockDeletionMarkIndexEntry(ctx context.Context, id ulid.ULID) (*BlockDeletionMark, error) {
	m := block.DeletionMark{}

	if err := block.ReadMarker(ctx, w.logger, w.bkt, id.String(), &m); err != nil {
		if errors.Is(err, block.ErrorMarkerNotFound) {
			return nil, errors.Wrap(ErrBlockDeletionMarkNotFound, err.Error())
		}
		if errors.Is(err, block.ErrorUnmarshalMarker) {
			return nil, errors.Wrap(ErrBlockDeletionMarkCorrupted, err.Error())
		}
		return nil, err
	}

	return BlockDeletionMarkFromThanosMarker(&m), nil
}
