package memsize

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/tarantool/go-iproto"

	xlogdir "github.com/tarantool/go-xlog/dir"
	"github.com/tarantool/go-xlog/format"
	"github.com/tarantool/go-xlog/reader"
)

const (
	spaceSchemaSpaceID = uint32(280)
	indexSchemaSpaceID = uint32(288)
	truncateSpaceID    = uint32(330)
)

// ErrNoSnapshot means the memtx directory has no numeric .snap file.
var ErrNoSnapshot = errors.New("no snapshot")

// Analyze estimates the recovered memtx footprint using one data directory
// for both snapshots and write-ahead logs.
func Analyze(ctx context.Context, dir string, cfg Config) (*Report, error) {
	return AnalyzeDirs(ctx, dir, dir, cfg)
}

// AnalyzeDirs estimates the recovered memtx footprint with separate snapshot
// and WAL directories.
func AnalyzeDirs(ctx context.Context, memtxDir, walDir string, cfg Config) (*Report, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("memsize: analyze: %w", err)
	}

	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("memsize: analyze config: %w", err)
	}

	snapDir, err := xlogdir.OpenDir(memtxDir, format.FiletypeSNAP)
	if err != nil {
		return nil, fmt.Errorf("memsize: open snapshot directory: %w", err)
	}

	files := snapDir.Files()
	if len(files) == 0 {
		return nil, fmt.Errorf("memsize: select snapshot in %q: %w", memtxDir, ErrNoSnapshot)
	}

	snapshot := files[len(files)-1]
	schema := BuildSchema()

	_, err = scanSnapshot(ctx, snapshot.Path, func(row format.XRow) error {
		if row.Type != iproto.IPROTO_INSERT {
			return nil
		}

		body, err := format.DecodeDMLBody(row.BodyRaw)
		if err != nil {
			return fmt.Errorf("decode schema DML body at LSN %d: %w", row.LSN, err)
		}

		switch body.SpaceID {
		case spaceSchemaSpaceID:
			if err := schema.ApplySpaceRow(row.Type, body.Tuple); err != nil {
				return fmt.Errorf("apply _space row at LSN %d: %w", row.LSN, err)
			}
		case indexSchemaSpaceID:
			if err := schema.ApplyIndexRow(row.Type, body.Tuple); err != nil {
				return fmt.Errorf("apply _index row at LSN %d: %w", row.LSN, err)
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("memsize: build schema: %w", err)
	}

	xlogFiles, err := selectXlogFiles(walDir, snapshot.VClock)
	if err != nil {
		return nil, fmt.Errorf("memsize: select xlogs: %w", err)
	}

	plan, err := buildReplayPlan(ctx, schema, xlogFiles, snapshot.VClock)
	if err != nil {
		return nil, fmt.Errorf("memsize: plan replay: %w", err)
	}

	accumulator := newSnapshotAccumulator(schema, normalized)

	state := &replayState{
		tuples:       make(map[replayKey]tupleMeta, len(plan.touched)),
		approximated: make(map[uint32]struct{}, len(plan.updates)),
	}
	for spaceID := range plan.updates {
		state.approximated[spaceID] = struct{}{}
	}

	var keyScratch primaryKeyScratch

	stats, err := scanSnapshot(ctx, snapshot.Path, func(row format.XRow) error {
		if row.Type != iproto.IPROTO_INSERT {
			return nil
		}

		body, err := format.DecodeDMLBody(row.BodyRaw)
		if err != nil {
			return fmt.Errorf("decode tuple DML body at LSN %d: %w", row.LSN, err)
		}

		meta, added, err := accumulator.addTuple(body.SpaceID, body.Tuple)
		if err != nil {
			return fmt.Errorf("size tuple in space %d at LSN %d: %w", body.SpaceID, row.LSN, err)
		}

		if !added {
			return nil
		}

		if _, touched := plan.touchedSpace[body.SpaceID]; !touched {
			return nil
		}

		space, ok := schema.Space(body.SpaceID)
		if !ok {
			return nil
		}

		hash, err := keyScratch.tupleHash(body.Tuple, space.PK())
		if err != nil {
			return fmt.Errorf("key snapshot tuple in space %q at LSN %d: %w", space.Name, row.LSN, err)
		}

		key := replayKey{spaceID: body.SpaceID, hash: hash}
		if _, touched := plan.touched[key]; touched {
			state.tuples[key] = meta.retain()
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("memsize: accumulate snapshot: %w", err)
	}

	replayStats, err := replayXlogs(ctx, schema, accumulator, state, xlogFiles, snapshot.VClock)
	if err != nil {
		return nil, fmt.Errorf("memsize: replay xlogs: %w", err)
	}

	for _, warning := range plan.warnings {
		accumulator.warn(warning.Kind, warning.Space, warning.Index, warning.Detail)
	}

	if plan.syncRollback {
		accumulator.markUpperBound()
		accumulator.warn(WarnSyncRollback, "", "",
			"qsync rollback was detected but not undone; tuple bytes are an upper bound")
	}

	approximated := make([]uint32, 0, len(state.approximated))
	for spaceID := range state.approximated {
		approximated = append(approximated, spaceID)
	}

	slices.Sort(approximated)

	for _, spaceID := range approximated {
		space, ok := schema.Space(spaceID)
		if !ok {
			continue
		}

		accumulator.warn(WarnUpdateApproximation, space.Name, "",
			"UPDATE and present-key UPSERT operations are treated as size-preserving")
	}

	tailTruncated := plan.tail || replayStats.tailTruncated
	if tailTruncated {
		accumulator.warn(WarnTailTruncated, "", "",
			"the last xlog has no EOF marker; any incomplete trailing transaction was omitted")
	}

	xlogPaths := make([]string, len(xlogFiles))
	for i := range xlogFiles {
		xlogPaths[i] = xlogFiles[i].Path
	}

	source := SourceInfo{
		SnapPath:      snapshot.Path,
		XlogPaths:     xlogPaths,
		FromVClock:    snapshot.VClock.Clone(),
		ToVClock:      replayStats.toVClock,
		Rows:          stats.rows + replayStats.rows,
		Txs:           stats.txs + replayStats.txs,
		TailTruncated: tailTruncated,
	}

	return accumulator.buildReport(source), nil
}

type snapshotScanStats struct {
	rows uint64
	txs  uint64
}

func scanSnapshot(ctx context.Context, path string, visit func(format.XRow) error) (snapshotScanStats, error) {
	journal, err := reader.Open(path, reader.WithAliasBodies())
	if err != nil {
		return snapshotScanStats{}, fmt.Errorf("open %q: %w", path, err)
	}

	var (
		stats   snapshotScanStats
		scanErr error
	)

	for journal.Scan() {
		if err := ctx.Err(); err != nil {
			scanErr = err

			break
		}

		row := journal.Row()

		stats.rows++
		if row.IsCommit() {
			stats.txs++
		}

		if err := visit(row); err != nil {
			scanErr = err

			break
		}
	}

	if scanErr == nil {
		scanErr = journal.Err()
	}

	closeErr := journal.Close()
	if scanErr != nil || closeErr != nil {
		return snapshotScanStats{}, fmt.Errorf("scan %q: %w", path, errors.Join(scanErr, closeErr))
	}

	return stats, nil
}
