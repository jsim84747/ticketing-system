-- All keys share an event hash tag. Redis serializes this script on the primary.
local waiting, active, started, generation, sequence, joined, metrics, config, reservations, stock = unpack(KEYS)
local action, user = ARGV[1], ARGV[2]
local now = tonumber(ARGV[3])
if now == 0 then
  local t = redis.call('TIME')
  now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end
local limit, timeout, maxstay = tonumber(ARGV[4]), tonumber(ARGV[5]), tonumber(ARGV[6])
local signature = ARGV[4] .. ':' .. ARGV[5] .. ':' .. ARGV[6]
redis.call('HSETNX',config,'signature',signature)
if redis.call('HGET',config,'signature') ~= signature then return redis.error_reply('CONFIG_MISMATCH') end
local function remove(id)
  redis.call('ZREM',active,id)
  redis.call('ZREM',started,id)
  redis.call('HDEL',generation,id)
end
if action == 'tick' then
  local room = math.max(0,limit-redis.call('ZCARD',active))
  local rows = {}
  if room > 0 then rows = redis.call('ZRANGE',waiting,0,math.min(room,1000)-1,'WITHSCORES') end
  local admitted = {}
  if room > 0 then
    for i=1,#rows,2 do
      local id, score = rows[i], tonumber(rows[i+1])
      redis.call('ZREM',waiting,id)
      redis.call('ZADD',active,now,id)
      redis.call('ZADD',started,now,id)
      redis.call('HSET',generation,id,string.format('%.0f',score))
      local wait = now-tonumber(redis.call('HGET',joined,id))
      redis.call('HDEL',joined,id)
      redis.call('HINCRBY',metrics,'admissions',1)
      redis.call('HINCRBY',metrics,'wait_total_ms',wait)
      local maxwait = tonumber(redis.call('HGET',metrics,'wait_max_ms') or '0')
      redis.call('HSET',metrics,'wait_max_ms',math.max(maxwait,wait))
      local last = tonumber(redis.call('HGET',metrics,'last_score') or '0')
      if score <= last then redis.call('HINCRBY',metrics,'order_violations',1) end
      redis.call('HSET',metrics,'last_score',string.format('%.0f',score))
      table.insert(admitted,id)
    end
  end
  local peak = tonumber(redis.call('HGET',metrics,'peak_active') or '0')
  redis.call('HSET',metrics,'peak_active',math.max(peak,redis.call('ZCARD',active)))
  if #admitted == 0 then return '[]' end
  return cjson.encode(admitted)
end
if action == 'leave' then
  remove(user)
  redis.call('ZREM',waiting,user)
  redis.call('HDEL',joined,user)
end
local function status()
  if redis.call('ZSCORE',active,user) then
    return cjson.encode({status='admitted',position=0,ahead=0,
      generation=redis.call('HGET',generation,user),
      expires_at=tonumber(redis.call('ZSCORE',started,user))+maxstay})
  end
  local rank = redis.call('ZRANK', waiting, user)
  if rank then
    return cjson.encode({status='waiting', position=rank+1, ahead=rank,
      ticket=string.format('%.0f',tonumber(redis.call('ZSCORE',waiting,user))), estimated_wait_seconds=cjson.null})
  end
  return cjson.encode({status='absent',position=0,ahead=0})
end
if action == 'join' then
  if not redis.call('ZSCORE',waiting,user) and not redis.call('ZSCORE',active,user) then
    -- Logical microsecond score: INCR also orders ties and clock regressions.
    local s = math.max(redis.call('INCR',sequence), now * 1000)
    redis.call('SET',sequence,string.format('%.0f',s))
    redis.call('ZADD',waiting,s,user)
    redis.call('HSET',joined,user,now)
  end
end
return status()
