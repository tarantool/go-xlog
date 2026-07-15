local fio = require('fio')
local json = require('json')

local datadir = assert(arg[1], 'data directory is required')
local scenario = assert(arg[2], 'scenario is required')
local action = assert(arg[3], 'action is required')
local replay_scenario = scenario == 'replay' or scenario == 'multikey_replay'

box.cfg({
    memtx_dir = datadir,
    wal_dir = datadir,
    vinyl_dir = datadir,
    memtx_memory = 512 * 1024 * 1024,
    slab_alloc_factor = 1.05,
    slab_alloc_granularity = 8,
    wal_mode = replay_scenario and 'write' or 'none',
    log = fio.pathjoin(datadir, 'tarantool.log'),
    pid_file = fio.pathjoin(datadir, 'tarantool.pid'),
})

local function insert_in_batches(space, count, make_tuple)
    local batch_size = 1000
    for first = 1, count, batch_size do
        local last = math.min(first + batch_size - 1, count)
        box.atomic(function()
            for i = first, last do
                space:insert(make_tuple(i))
            end
        end)
    end
end

local function run_in_batches(first, last, action)
    local batch_size = 500
    for batch_first = first, last, batch_size do
        local batch_last = math.min(batch_first + batch_size - 1, last)
        box.atomic(function()
            for i = batch_first, batch_last do
                action(i)
            end
        end)
    end
end

local function create_calibration()
    local hinted = box.schema.space.create('cal_tree_hinted', {
        format = {
            {name = 'id', type = 'unsigned'},
            {name = 'group_id', type = 'unsigned'},
            {name = 'name', type = 'string'},
            {name = 'payload', type = 'string'},
        },
    })
    hinted:create_index('primary', {parts = {{field = 1, type = 'unsigned'}}})
    hinted:create_index('by_name', {
        unique = true,
        parts = {{field = 3, type = 'string'}},
    })
    hinted:create_index('by_group', {
        unique = false,
        hint = false,
        parts = {{field = 2, type = 'unsigned'}},
    })
    insert_in_batches(hinted, 12000, function(i)
        return {i, i % 97, string.format('name-%05d', i), string.rep('x', (i * 17) % 500)}
    end)

    local plain = box.schema.space.create('cal_tree_plain')
    plain:create_index('primary', {
        hint = false,
        parts = {{field = 1, type = 'unsigned'}},
    })
    insert_in_batches(plain, 7000, function(i)
        return {i, string.rep('p', (i * 11) % 180)}
    end)

    local hashed = box.schema.space.create('cal_hash')
    hashed:create_index('primary', {
        type = 'hash',
        parts = {{field = 1, type = 'unsigned'}},
    })
    insert_in_batches(hashed, 5000, function(i)
        return {i, string.rep('h', (i * 7) % 96)}
    end)

    local multipart = box.schema.space.create('cal_multipart')
    multipart:create_index('primary', {
        parts = {
            {field = 1, type = 'unsigned'},
            {field = 2, type = 'unsigned'},
        },
    })
    multipart:create_index('by_high_field', {
        unique = false,
        parts = {{field = 4, type = 'string'}},
    })
    insert_in_batches(multipart, 4000, function(i)
        return {math.floor((i - 1) / 10), i % 10, i % 31, string.format('value-%05d', i)}
    end)
end

local function create_heldout()
    local tree = box.schema.space.create('held_tree', {
        format = {
            {name = 'id', type = 'unsigned'},
            {name = 'bucket', type = 'unsigned'},
            {name = 'code', type = 'string'},
            {name = 'payload', type = 'string'},
            {name = 'tail', type = 'unsigned'},
        },
    })
    tree:create_index('primary', {parts = {{field = 1, type = 'unsigned'}}})
    tree:create_index('by_tail_code', {
        unique = false,
        hint = false,
        parts = {
            {field = 5, type = 'unsigned'},
            {field = 3, type = 'string'},
        },
    })
    insert_in_batches(tree, 23457, function(i)
        return {
            i,
            i % 211,
            string.format('code-%05d', i % 17003),
            string.rep('z', (i * 43) % 2048),
            i % 997,
        }
    end)

    local hashed = box.schema.space.create('held_hash')
    hashed:create_index('primary', {
        type = 'hash',
        parts = {{field = 1, type = 'unsigned'}},
    })
    insert_in_batches(hashed, 8193, function(i)
        return {i, string.rep('q', (i * 29) % 300)}
    end)

    local local_space = box.schema.space.create('held_local', {is_local = true})
    local_space:create_index('primary', {parts = {{field = 1, type = 'unsigned'}}})
    insert_in_batches(local_space, 3456, function(i)
        return {i, i % 17, string.rep('l', (i * 13) % 128)}
    end)

end

local function create_replay()
    local tree = box.schema.space.create('replay_tree', {
        format = {
            {name = 'id', type = 'unsigned'},
            {name = 'bucket', type = 'unsigned'},
            {name = 'payload', type = 'string'},
        },
    })
    tree:create_index('primary', {parts = {{field = 1, type = 'unsigned'}}})
    tree:create_index('by_bucket', {
        unique = false,
        hint = false,
        parts = {{field = 2, type = 'unsigned'}},
    })
    insert_in_batches(tree, 6000, function(i)
        return {i, i % 97, string.rep('s', (i * 19) % 256)}
    end)

    local hash = box.schema.space.create('replay_hash')
    hash:create_index('primary', {
        type = 'hash',
        parts = {{field = 1, type = 'unsigned'}},
    })
    insert_in_batches(hash, 4096, function(i)
        return {i, string.rep('h', (i * 7) % 128)}
    end)

    local truncated = box.schema.space.create('replay_truncate')
    truncated:create_index('primary', {parts = {{field = 1, type = 'unsigned'}}})
    insert_in_batches(truncated, 2000, function(i)
        return {i, string.rep('t', (i * 11) % 192)}
    end)
end

local function multikey_items(i, cardinality)
    local items = {}
    for j = 1, cardinality do
        local item = {
            value = i * 16 + j,
        }
        if (i + j) % 4 ~= 0 then
            item.code = string.format('code-%d-%d', i, j)
        end
        table.insert(items, item)
    end
    return items
end

local function create_multikey()
    local docs = box.schema.space.create('multikey_docs', {
        format = {
            {name = 'id', type = 'unsigned'},
            {name = 'doc', type = 'map'},
            {name = 'optional', type = 'string', is_nullable = true},
        },
    })
    docs:create_index('primary', {
        parts = {{field = 1, type = 'unsigned'}},
    })
    docs:create_index('by_value', {
        unique = false,
        parts = {{
            field = 2,
            type = 'unsigned',
            path = '.items[*].value',
            is_nullable = true,
        }},
    })
    docs:create_index('by_code_value', {
        unique = false,
        parts = {
            {
                field = 2,
                type = 'string',
                path = '.items[*].code',
                exclude_null = true,
            },
            {
                field = 2,
                type = 'unsigned',
                path = '.items[*].value',
                is_nullable = true,
            },
        },
    })
    docs:create_index('by_optional', {
        unique = false,
        parts = {{
            field = 3,
            type = 'string',
            exclude_null = true,
        }},
    })

    insert_in_batches(docs, 5000, function(i)
        local optional = i % 3 == 0 and box.NULL or string.format('optional-%d', i)
        return {i, {items = multikey_items(i, i % 7)}, optional}
    end)

    local truncated = box.schema.space.create('multikey_truncate')
    truncated:create_index('primary', {
        parts = {{field = 1, type = 'unsigned'}},
    })
    truncated:create_index('by_item', {
        unique = false,
        parts = {{
            field = 2,
            type = 'unsigned',
            path = '[*]',
        }},
    })
    insert_in_batches(truncated, 1000, function(i)
        return {i, {i * 2, i * 2 + 1}}
    end)
end

local function mutate_multikey()
    local docs = box.space.multikey_docs
    run_in_batches(1, 500, function(i)
        local optional = i % 2 == 0 and box.NULL or string.format('replacement-%d', i)
        docs:replace({i, {items = multikey_items(i + 10000, (i * 3) % 9)}, optional})
    end)
    run_in_batches(501, 800, function(i)
        docs:delete({i})
    end)
    run_in_batches(5001, 5500, function(i)
        local optional = i % 5 == 0 and box.NULL or string.format('inserted-%d', i)
        docs:insert({i, {items = multikey_items(i, (i * 5) % 8)}, optional})
    end)

    local truncated = box.space.multikey_truncate
    truncated:truncate()
    insert_in_batches(truncated, 300, function(i)
        return {i, {i, i + 1, i + 2}}
    end)
end

local function mutate_replay()
    local tree = box.space.replay_tree
    run_in_batches(1, 1000, function(i)
        tree:replace({i, i % 97, string.rep('r', (i * 31) % 800)})
    end)
    run_in_batches(1001, 1500, function(i)
        tree:delete({i})
    end)
    run_in_batches(6001, 6500, function(i)
        tree:insert({i, i % 97, string.rep('n', (i * 23) % 384)})
    end)
    run_in_batches(1501, 1600, function(i)
        tree:update({i}, {{'+', 2, 0}})
    end)
    run_in_batches(1601, 1700, function(i)
        tree:upsert({i, 0, 'ignored'}, {{'+', 2, 0}})
    end)
    run_in_batches(7001, 7100, function(i)
        tree:upsert({i, i % 97, string.rep('u', (i * 13) % 320)}, {{'+', 2, 0}})
    end)

    local hash = box.space.replay_hash
    run_in_batches(1, 512, function(i)
        hash:delete({i})
    end)
    run_in_batches(4097, 4352, function(i)
        hash:insert({i, string.rep('q', (i * 17) % 160)})
    end)

    local truncated = box.space.replay_truncate
    truncated:truncate()
    insert_in_batches(truncated, 400, function(i)
        return {i, string.rep('a', (i * 29) % 224)}
    end)
end

local function persistent_memtx_space(definition)
    local engine = definition[4]
    local opts = definition[6]
    local kind = opts.type
    return engine == 'memtx' and
        kind ~= 'data-temporary' and
        kind ~= 'temporary' and
        opts.temporary ~= true and
        opts.view ~= true and
        opts.is_view ~= true
end

local function record_oracle()
    local info = box.slab.info()
    local result = {
        scenario = scenario,
        arena_used = tonumber(info.arena_used),
        quota_used = tonumber(info.quota_used),
        spaces = {},
        slab_stats = {},
    }

    for _, stat in ipairs(box.slab.stats()) do
        table.insert(result.slab_stats, {
            item_size = tonumber(stat.item_size),
            item_count = tonumber(stat.item_count),
            mem_used = tonumber(stat.mem_used),
        })
    end

    for _, definition in box.space._space:pairs() do
        if persistent_memtx_space(definition) then
            local id = definition[1]
            local space = box.space[id]
            local tuple_stat = space:stat().tuple.memtx
            local space_result = {
                id = id,
                name = definition[3],
                payload_bytes = tonumber(space:bsize()),
                tuple_count = space:len(),
                tuple_header_bytes = tonumber(tuple_stat.header_size),
                tuple_field_map_bytes = tonumber(tuple_stat.field_map_size),
                tuple_waste_bytes = tonumber(tuple_stat.waste_size),
                indexes = {},
            }

            for _, index_definition in box.space._index:pairs() do
                if index_definition[1] == id then
                    local index = space.index[index_definition[2]]
                    table.insert(space_result.indexes, {
                        id = index.id,
                        name = index.name,
                        entries = index:count(),
                        bytes = tonumber(index:bsize()),
                    })
                end
            end

            table.sort(space_result.indexes, function(left, right)
                return left.id < right.id
            end)
            table.insert(result.spaces, space_result)
        end
    end

    table.sort(result.spaces, function(left, right)
        return left.id < right.id
    end)

    local output = assert(io.open(fio.pathjoin(datadir, 'oracle.json'), 'w'))
    assert(output:write(json.encode(result)))
    assert(output:close())
end

if action == 'generate' then
    box.once('memsize-oracle-' .. scenario, function()
        if scenario == 'calibration' then
            create_calibration()
        elseif scenario == 'heldout' then
            create_heldout()
        elseif scenario == 'replay' then
            create_replay()
        elseif scenario == 'multikey' or scenario == 'multikey_replay' then
            create_multikey()
        else
            error('unknown scenario: ' .. scenario)
        end
    end)
    box.snapshot()
    if replay_scenario then
        if scenario == 'replay' then
            mutate_replay()
        else
            mutate_multikey()
        end
        record_oracle()
    end
elseif action == 'observe' then
    record_oracle()
else
    error('unknown action: ' .. action)
end

os.exit(0)
