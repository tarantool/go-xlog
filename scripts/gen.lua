#!/usr/bin/env tarantool
-- Generate Tarantool 3.x xlog/snap/vylog fixtures.
-- Invoked by scripts/gen-fixtures.sh with arg[1] = work_dir.

local work_dir = arg[1]
if not work_dir or work_dir == '' then
    io.stderr:write('usage: tarantool gen.lua <work_dir>\n')
    os.exit(1)
end

box.cfg{
    work_dir = work_dir,
    memtx_use_mvcc_engine = false,
    listen = nil,
    log = '/dev/null',
    wal_mode = 'write',
}

-- Bootstrap snap (signature 0) has already been written by box.cfg{}.
-- We use it as the "empty.snap" fixture.

-- Create a memtx space for the xlog content.
local s = box.schema.space.create('test', { if_not_exists = true })
s:create_index('pk', { parts = {1, 'unsigned'}, if_not_exists = true })

-- Create a vinyl space + insert a row so the vylog gets non-trivial content
-- (LSM creation + range/run records on dump). Snapshot triggers a dump.
local v = box.schema.space.create('vtest', {
    engine = 'vinyl',
    if_not_exists = true,
})
v:create_index('pk', { parts = {1, 'unsigned'}, if_not_exists = true })

-- 3 single-statement inserts on memtx (small tuples).
s:insert{1, 'alpha'}
s:insert{2, 'beta'}
s:insert{3, 'gamma'}

-- 1 multi-row transaction (3 statements).
box.begin()
s:insert{10, 'tx-one'}
s:insert{11, 'tx-two'}
s:insert{12, 'tx-three'}
box.commit()

-- 1 insert with a tuple >= 4 KiB to push the tx payload past the 2 KiB
-- XLOG_TX_COMPRESS_THRESHOLD and force a zstd-compressed tx block.
local big = string.rep('x', 4096)
s:insert{100, big}

-- A vinyl insert to make the vylog meaningful.
v:insert{1, 'vinyl-row'}

-- Take a snapshot now that data has been written. Tarantool will create a
-- new <signature>.snap reflecting the post-insert state, and dump the vinyl
-- index (which writes records to the vylog).
box.snapshot()

os.exit(0)
