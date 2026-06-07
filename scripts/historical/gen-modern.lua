#!/usr/bin/env tarantool
-- Modern historical-corpus generator for Tarantool 2.11+ (format 0.13).
-- Used by scripts/gen-historical.sh for the 2.x / 3.x lineages.
--
-- Synchronous replication, Raft election, and vinyl are pcall-guarded so the
-- script stays robust across the supported range even though every covered
-- version produces the full feature set.
--
-- arg[1] = work_dir (defaults to /wd for the in-container run).

local wd = arg[1] or '/wd'

box.cfg{
    work_dir         = wd,
    memtx_dir        = wd,
    wal_dir          = wd,
    vinyl_dir        = wd,
    log              = wd .. '/tt.log',
    checkpoint_count = 4,
}

-- Raft election + synchronous replication (2.6+). On older versions these
-- cfg keys do not exist; pcall swallows the error.
pcall(box.cfg, {
    election_mode               = 'candidate',
    replication_synchro_quorum  = 1,
    replication_synchro_timeout = 1,
    replication_timeout         = 0.4,
})
pcall(function() box.ctl.wait_rw(5) end)

-- memtx space, fixed id for a deterministic golden.
local s = box.schema.space.create('test', { id = 512, if_not_exists = true })
s:create_index('pk', { parts = {1, 'unsigned'}, if_not_exists = true })

-- Full single-statement DML coverage.
s:insert{1, 'alpha'}
s:insert{2, 'beta'}
s:replace{2, 'beta2'}
s:update({1}, {{'=', 2, 'alpha2'}})
s:upsert({3, 'gamma'}, {{'=', 2, 'gamma2'}})
s:delete({2})

-- Multi-statement transaction (tsn/is_commit grouping).
box.begin()
s:insert{10, 'tx-a'}
s:insert{11, 'tx-b'}
s:insert{12, 'tx-c'}
box.commit()

-- Large tuple → zstd-compressed tx block (> 2 KiB threshold).
s:insert{100, string.rep('z', 4096)}

-- Synchronous space → CONFIRM (SYNCHRO) rows at quorum=1 (2.6+). Guarded.
pcall(function()
    local sy = box.schema.space.create('sync', { id = 513, is_sync = true })
    sy:create_index('pk', { parts = {1, 'unsigned'} })
    sy:insert{1, 'synced'}
end)

-- Vinyl space → .vylog / .run / .index on snapshot dump (1.7+). Guarded.
pcall(function()
    local v = box.schema.space.create('vin', { id = 514, engine = 'vinyl' })
    v:create_index('pk', { parts = {1, 'unsigned'} })
    v:insert{1, 'vin-a'}
    v:insert{2, 'vin-b'}
end)

-- Snapshot: populated .snap + vinyl dump (run/index + vylog records).
box.snapshot()

-- Post-snapshot inserts → a second xlog so the directory has a >=2 file chain.
s:insert{200, 'post-snap-1'}
s:insert{201, 'post-snap-2'}

os.exit(0)
