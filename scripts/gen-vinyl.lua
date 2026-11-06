#!/usr/bin/env tarantool
-- Generate vinyl artefacts: .run and .index files of a primary and a secondary
-- index holding every statement type a run carries (INSERT, REPLACE, DELETE,
-- UPSERT), deferred DELETEs, a zstd-compressed page and a secondary index of
-- several pages, plus the .vylog that registers them.
-- Invoked by scripts/gen-fixtures.sh with arg[1] = work_dir.

local fiber = require('fiber')

local work_dir = arg[1]
if not work_dir or work_dir == '' then
    io.stderr:write('usage: tarantool gen-vinyl.lua <work_dir>\n')
    os.exit(1)
end

box.cfg{
    work_dir = work_dir,
    log = work_dir .. '/tarantool.log',
    -- Keep the runs compaction replaces: every checkpoint references them.
    checkpoint_count = 100,
}

-- defer_deletes: a REPLACE/DELETE in the primary index does not look up the
-- old tuple; compaction of the primary index generates the DELETEs for the
-- secondary index instead.
local s = box.schema.space.create('vin', {
    id = 700, engine = 'vinyl', defer_deletes = true,
})
s:create_index('pk', { parts = {1, 'unsigned'} })
-- A tiny page size gives the secondary index runs of several pages.
s:create_index('sk', { parts = {2, 'string'}, page_size = 64 })

-- A space with a secondary index executes an UPSERT as a read and a REPLACE;
-- a space of only a primary index keeps it an UPSERT down to its runs.
local u = box.schema.space.create('ups', { id = 701, engine = 'vinyl' })
u:create_index('pk', { parts = {1, 'unsigned'} })

-- Dump the memory and wait for the compactions the dump schedules: vinyl
-- keeps at most one run at the last level of an LSM tree, so every dump of
-- an index that has a run is followed by a compaction.
local function dump()
    box.snapshot()
    local sched
    repeat
        fiber.sleep(0.05)
        sched = box.stat.vinyl().scheduler
    until sched.tasks_inprogress == 0 and sched.compaction_queue == 0
end

-- Dump 1: the first run of each index.
s:insert{1, 'a'}
s:insert{2, 'b'}
s:insert{3, 'c'}
s:insert{4, 'd'}
u:insert{1, 0}
dump()

-- Dump 2: a run above the first one keeps DELETEs and UPSERTs as they are;
-- the compaction after it applies them. Compaction of the primary index of
-- vin generates the deferred DELETEs of its secondary index.
s:replace{1, 'a2'}                      -- REPLACE, deferred DELETE of {1, 'a'}
s:delete{2}                             -- DELETE, deferred DELETE of {2, 'b'}
s:insert{5, 'e', string.rep('z', 3000)} -- compaction compresses this page
u:upsert({1, 0}, {{'+', 2, 1}})
u:upsert({1, 0}, {{'+', 2, 10}, {'=', 3, 'x'}}) -- squashes with the one above
u:upsert({2, 0}, {{'+', 2, 1}})         -- an UPSERT of a key no run holds
dump()

-- Dump 3: the deferred DELETEs of the secondary index.
dump()

-- Dump 4: more DELETE and UPSERT statements.
s:delete{4}
u:upsert({1, 0}, {{'+', 2, 100}})
u:delete{2}
dump()

os.exit(0)
