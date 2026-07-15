package memsize

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/tarantool/go-iproto"

	xlogdir "github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/filter"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

// Replay errors are comparable sentinels for errors.Is.
var (
	ErrChainGap     = errors.New("xlog chain gap")
	ErrNoPrimaryKey = errors.New("memtx space has no primary key")
)

type replayKey struct {
	spaceID uint32
	hash    uint64
}

type replayPlan struct {
	touched      map[replayKey]struct{}
	touchedSpace map[uint32]struct{}
	warnings     []Warning
	warningKeys  map[string]struct{}
	updates      map[uint32]struct{}
	syncRollback bool
	tail         bool
}

type replayState struct {
	tuples       map[replayKey]tupleMeta
	approximated map[uint32]struct{}
}

type xlogScanStats struct {
	rows          uint64
	txs           uint64
	toVClock      format.VClock
	tailTruncated bool
}

func selectXlogFiles(walDir string, snapshotVClock format.VClock) ([]xlogdir.FileEntry, error) {
	directory, err := xlogdir.OpenDir(walDir, format.FiletypeXLOG)
	if err != nil {
		return nil, fmt.Errorf("open WAL directory: %w", err)
	}

	files := directory.Files()
	if len(files) == 0 {
		return nil, nil
	}

	first, err := directory.LocateVClock(snapshotVClock)
	if err != nil {
		if errors.Is(err, xlogdir.ErrNotFound) {
			return nil, fmt.Errorf("%w: no xlog straddles snapshot vclock %s", ErrChainGap, snapshotVClock)
		}

		return nil, fmt.Errorf("locate snapshot vclock %s: %w", snapshotVClock, err)
	}

	start := slices.IndexFunc(files, func(file xlogdir.FileEntry) bool {
		return file.Path == first.Path
	})
	if start < 0 {
		return nil, fmt.Errorf("%w: located xlog %q is absent from the WAL directory index",
			ErrChainGap, first.Path)
	}

	selected := files[start:]
	for fileNo := 1; fileNo < len(selected); fileNo++ {
		previous := &selected[fileNo-1]
		current := &selected[fileNo]

		order, ok := current.PrevVClock.Compare(previous.VClock)
		if !ok || order != 0 {
			return nil, fmt.Errorf("%w: %q PrevVClock=%s, previous %q VClock=%s",
				ErrChainGap, current.Path, current.PrevVClock, previous.Path, previous.VClock)
		}
	}

	return selected, nil
}

func buildReplayPlan(
	ctx context.Context,
	schema *Schema,
	files []xlogdir.FileEntry,
	from format.VClock,
) (*replayPlan, error) {
	plan := &replayPlan{
		touched:      make(map[replayKey]struct{}),
		touchedSpace: make(map[uint32]struct{}),
		warningKeys:  make(map[string]struct{}),
		updates:      make(map[uint32]struct{}),
	}

	var keyScratch primaryKeyScratch

	stats, err := scanXlogs(ctx, files, from, func(row format.XRow) error {
		return plan.observeRow(schema, row, &keyScratch)
	})
	if err != nil {
		return nil, err
	}

	plan.tail = stats.tailTruncated

	return plan, nil
}

func (p *replayPlan) observeRow(schema *Schema, row format.XRow, keyScratch *primaryKeyScratch) error {
	if row.Type == iproto.IPROTO_RAFT_ROLLBACK {
		p.syncRollback = true

		return nil
	}

	if !isReplayDML(row.Type) {
		return nil
	}

	body, err := format.DecodeDMLBody(row.BodyRaw)
	if err != nil {
		return fmt.Errorf("decode %s body at LSN %d: %w", row.Type, row.LSN, err)
	}

	switch body.SpaceID {
	case spaceSchemaSpaceID, indexSchemaSpaceID:
		return p.observeSchemaDDL(schema, row, body)
	case truncateSpaceID:
		if _, err := truncateTarget(schema, row.Type, body); err != nil {
			return fmt.Errorf("truncate at LSN %d: %w", row.LSN, err)
		}
	}

	space, ok := schema.Space(body.SpaceID)
	if !ok || !space.IsMemtxData() {
		return nil
	}

	hash, err := replayRowKey(row.Type, body, space.PK(), keyScratch)
	if err != nil {
		return fmt.Errorf("space %q row at LSN %d: %w", space.Name, row.LSN, err)
	}

	key := replayKey{spaceID: body.SpaceID, hash: hash}
	p.touched[key] = struct{}{}
	p.touchedSpace[body.SpaceID] = struct{}{}

	if row.Type == iproto.IPROTO_UPDATE {
		p.updates[body.SpaceID] = struct{}{}
	}

	return nil
}

func (p *replayPlan) observeSchemaDDL(schema *Schema, row format.XRow, body *format.DMLBody) error {
	raw := body.Tuple
	if len(raw) == 0 {
		raw = body.Key
	}

	spaceID, err := decodeFirstUint32(raw, ErrInvalidSpaceRow)
	if err != nil {
		return fmt.Errorf("decode schema object at LSN %d: %w", row.LSN, err)
	}

	spaceName := fmt.Sprintf("space_id=%d", spaceID)
	if space, ok := schema.Space(spaceID); ok {
		spaceName = space.Name
	}

	indexName := ""
	object := "_space"

	if body.SpaceID == indexSchemaSpaceID {
		_, indexID, err := decodeIndexKey(raw)
		if err != nil {
			return fmt.Errorf("decode index object at LSN %d: %w", row.LSN, err)
		}

		object, indexName = "_index", fmt.Sprintf("index_id=%d", indexID)

		if space, ok := schema.Space(spaceID); ok {
			if pos, found := findIndex(space.Indexes, indexID); found {
				indexName = space.Indexes[pos].Name
			}
		}
	}

	warningKey := fmt.Sprintf("%d/%s/%s", body.SpaceID, spaceName, indexName)
	if _, duplicate := p.warningKeys[warningKey]; duplicate {
		return nil
	}

	p.warningKeys[warningKey] = struct{}{}
	p.warnings = append(p.warnings, Warning{
		Kind:   WarnSchemaDDL,
		Space:  spaceName,
		Index:  indexName,
		Detail: fmt.Sprintf("%s on %s after the snapshot is detected but not replayed", row.Type, object),
	})

	return nil
}

func replayXlogs(
	ctx context.Context,
	schema *Schema,
	accumulator *snapshotAccumulator,
	state *replayState,
	files []xlogdir.FileEntry,
	from format.VClock,
) (xlogScanStats, error) {
	var keyScratch primaryKeyScratch

	return scanXlogs(ctx, files, from, func(row format.XRow) error {
		if row.Type == iproto.IPROTO_RAFT_ROLLBACK || !isReplayDML(row.Type) {
			return nil
		}

		body, err := format.DecodeDMLBody(row.BodyRaw)
		if err != nil {
			return fmt.Errorf("decode %s body at LSN %d: %w", row.Type, row.LSN, err)
		}

		if body.SpaceID == spaceSchemaSpaceID || body.SpaceID == indexSchemaSpaceID {
			return nil
		}

		space, ok := schema.Space(body.SpaceID)
		if !ok || !space.IsMemtxData() {
			return nil
		}

		if err := applyReplayDML(accumulator, state, space, row.Type, body, &keyScratch); err != nil {
			return fmt.Errorf("space %q row at LSN %d: %w", space.Name, row.LSN, err)
		}

		if body.SpaceID == truncateSpaceID && row.Type != iproto.IPROTO_DELETE {
			target, err := truncateTarget(schema, row.Type, body)
			if err != nil {
				return fmt.Errorf("truncate at LSN %d: %w", row.LSN, err)
			}

			accumulator.clearSpace(target)

			for key := range state.tuples {
				if key.spaceID == target {
					delete(state.tuples, key)
				}
			}
		}

		return nil
	})
}

func applyReplayDML(
	accumulator *snapshotAccumulator,
	state *replayState,
	space *Space,
	typeID iproto.Type,
	body *format.DMLBody,
	keyScratch *primaryKeyScratch,
) error {
	hash, err := replayRowKey(typeID, body, space.PK(), keyScratch)
	if err != nil {
		return err
	}

	key := replayKey{spaceID: body.SpaceID, hash: hash}
	old, exists := state.tuples[key]

	switch typeID {
	case iproto.IPROTO_INSERT, iproto.IPROTO_REPLACE:
		replacement, ok, err := accumulator.allocateTuple(body.SpaceID, body.Tuple)
		if err != nil {
			return err
		}

		if !ok {
			return nil
		}

		if exists {
			accumulator.replaceTuple(body.SpaceID, old, replacement)
		} else {
			accumulator.addAllocatedTuple(body.SpaceID, replacement, true)
		}

		state.tuples[key] = replacement.retain()
	case iproto.IPROTO_DELETE:
		if exists {
			accumulator.removeTuple(body.SpaceID, old)
			delete(state.tuples, key)
		}
	case iproto.IPROTO_UPDATE:
		if exists {
			accumulator.markSpaceChurn(body.SpaceID)
			state.approximated[body.SpaceID] = struct{}{}
		}
	case iproto.IPROTO_UPSERT:
		if exists {
			accumulator.markSpaceChurn(body.SpaceID)
			state.approximated[body.SpaceID] = struct{}{}

			return nil
		}

		inserted, ok, err := accumulator.allocateTuple(body.SpaceID, body.Tuple)
		if err != nil {
			return err
		}

		if !ok {
			return nil
		}

		accumulator.addAllocatedTuple(body.SpaceID, inserted, true)
		state.tuples[key] = inserted.retain()
	}

	return nil
}

func replayRowKey(
	typeID iproto.Type,
	body *format.DMLBody,
	pk *Index,
	keyScratch *primaryKeyScratch,
) (uint64, error) {
	if pk == nil {
		return 0, ErrNoPrimaryKey
	}

	switch typeID {
	case iproto.IPROTO_INSERT, iproto.IPROTO_REPLACE, iproto.IPROTO_UPSERT:
		if len(body.Tuple) == 0 {
			return 0, fmt.Errorf("%w: %s body has no tuple", ErrInvalidPrimaryKey, typeID)
		}

		return keyScratch.tupleHash(body.Tuple, pk)
	case iproto.IPROTO_UPDATE, iproto.IPROTO_DELETE:
		if len(body.Key) == 0 {
			return 0, fmt.Errorf("%w: %s body has no key", ErrInvalidPrimaryKey, typeID)
		}

		return keyScratch.keyHash(body.Key, pk)
	default:
		return 0, fmt.Errorf("%w: unsupported replay type %s", ErrInvalidPrimaryKey, typeID)
	}
}

func truncateTarget(schema *Schema, typeID iproto.Type, body *format.DMLBody) (uint32, error) {
	raw := body.Tuple
	if len(raw) == 0 {
		raw = body.Key
	}

	if err := schema.ApplyTruncateRow(typeID, raw); err != nil {
		return 0, err
	}

	return decodeFirstUint32(raw, ErrInvalidTruncateRow)
}

func isReplayDML(typeID iproto.Type) bool {
	switch typeID {
	case iproto.IPROTO_INSERT,
		iproto.IPROTO_REPLACE,
		iproto.IPROTO_UPDATE,
		iproto.IPROTO_UPSERT,
		iproto.IPROTO_DELETE:
		return true
	default:
		return false
	}
}

func scanXlogs(
	ctx context.Context,
	files []xlogdir.FileEntry,
	from format.VClock,
	visit func(format.XRow) error,
) (xlogScanStats, error) {
	stats := xlogScanStats{toVClock: from.Clone()}
	boundary := filter.FromVClock(from)

	for fileNo := range files {
		last := fileNo == len(files)-1

		options := []reader.Option{reader.WithAliasBodies()}
		if last {
			options = append(options, reader.IgnoreMissingEOF())
		}

		journal, err := reader.Open(files[fileNo].Path, options...)
		if err != nil {
			return xlogScanStats{}, fmt.Errorf("open %q: %w", files[fileNo].Path, err)
		}

		var scanErr error

		for journal.ScanTx() {
			if err := ctx.Err(); err != nil {
				scanErr = err

				break
			}

			kept := false

			for _, row := range journal.Tx() {
				if !boundary(row) {
					continue
				}

				kept = true

				stats.rows++
				if row.LSN > stats.toVClock[row.ReplicaID] {
					stats.toVClock[row.ReplicaID] = row.LSN
				}

				if err := visit(row); err != nil {
					scanErr = err

					break
				}
			}

			if kept {
				stats.txs++
			}

			if scanErr != nil {
				break
			}
		}

		if scanErr == nil {
			scanErr = journal.Err()
		}

		if last && errors.Is(scanErr, reader.ErrTruncated) {
			stats.tailTruncated = true
			scanErr = nil
		}

		if last && !journal.SawEOFMarker() {
			stats.tailTruncated = true
		}

		closeErr := journal.Close()
		if scanErr != nil || closeErr != nil {
			return xlogScanStats{}, fmt.Errorf("scan %q: %w",
				files[fileNo].Path, errors.Join(scanErr, closeErr))
		}
	}

	return stats, nil
}
