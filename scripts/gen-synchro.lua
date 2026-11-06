#!/usr/bin/env tarantool
-- Generate journals with every synchronous replication row type: an xlog with
-- PROMOTE, CONFIRM, ROLLBACK and DEMOTE rows, and a snapshot whose limbo
-- checkpoint is a PROMOTE carrying the confirmed vclock of the queue.
-- Invoked by scripts/gen-fixtures.sh with arg[1] = work_dir.

local fiber = require('fiber')

local work_dir = arg[1]
if not work_dir or work_dir == '' then
    io.stderr:write('usage: tarantool gen-synchro.lua <work_dir>\n')
    os.exit(1)
end

box.cfg{
    work_dir = work_dir,
    log = work_dir .. '/tarantool.log',
    election_mode = 'off',
    replication_synchro_quorum = 1,
    replication_synchro_timeout = 1,
}

-- The first PROMOTE: the queue had no owner.
box.ctl.promote()

local s = box.schema.space.create('sync', { id = 600, is_sync = true })
s:create_index('pk')

-- A synchronous transaction confirmed at quorum 1: CONFIRM.
s:insert{1, 'confirmed'}

-- A synchronous transaction that cannot gather a quorum of 2 within the
-- timeout: ROLLBACK.
box.cfg{ replication_synchro_quorum = 2, replication_synchro_timeout = 0.1 }
pcall(s.insert, s, {2, 'rolled back'})
box.cfg{ replication_synchro_quorum = 1, replication_synchro_timeout = 1 }

s:insert{3, 'confirmed'}

-- The snapshot checkpoints the limbo as a PROMOTE with the confirmed vclock.
box.snapshot()

-- DEMOTE and PROMOTE again, in a new term each.
box.ctl.demote()
box.ctl.promote()
s:insert{4, 'confirmed'}

fiber.sleep(0.1)
os.exit(0)
