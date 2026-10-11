-- All keys share an event hash tag. Redis serializes this script on the primary.
local waiting, active, started, generation, sequence, joined, metrics, config, reservations, stock = unpack(KEYS)
local action, user = ARGV[1], ARGV[2]
local now = tonumber(ARGV[3])
if now == 0 then
  local t = redis.call('TIME')
  now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end
local function status()
  local rank = redis.call('ZRANK', waiting, user)
  if rank then
    return cjson.encode({status='waiting', position=rank+1, ahead=rank,
      ticket=string.format('%.0f',tonumber(redis.call('ZSCORE',waiting,user))), estimated_wait_seconds=cjson.null})
  end
  return cjson.encode({status='absent',position=0,ahead=0})
end
if action == 'join' then
  if not redis.call('ZSCORE',waiting,user) then
    -- Logical microsecond score: INCR also orders ties and clock regressions.
    local s = math.max(redis.call('INCR',sequence), now * 1000)
    redis.call('SET',sequence,string.format('%.0f',s))
    redis.call('ZADD',waiting,s,user)
    redis.call('HSET',joined,user,now)
  end
end
return status()
