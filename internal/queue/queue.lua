-- All keys share an event hash tag. Redis serializes this script on the primary.
local waiting, active, started, generation, sequence, joined, metrics, config, reservations, stock = unpack(KEYS)
local action, user = ARGV[1], ARGV[2]
local now = tonumber(ARGV[3])
if now == 0 then
  local t = redis.call('TIME')
  now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
end
-- Settings live in Redis so admins can change them at runtime and every server
-- reads the same values. ARGV only seeds the settings of a new event.
redis.call('HSETNX',config,'limit',ARGV[4])
redis.call('HSETNX',config,'timeout',ARGV[5])
redis.call('HSETNX',config,'maxstay',ARGV[6])
redis.call('HSETNX',config,'mode','auto')
local limit = tonumber(redis.call('HGET',config,'limit'))
local timeout = tonumber(redis.call('HGET',config,'timeout'))
local maxstay = tonumber(redis.call('HGET',config,'maxstay'))
local mode = redis.call('HGET',config,'mode')
local name = redis.call('HGET',config,'name') or ''
if action == 'stats' then
  local function metric(key) return tonumber(redis.call('HGET',metrics,key) or '0') end
  return cjson.encode({active=redis.call('ZCARD',active),waiting=redis.call('ZCARD',waiting),
    admissions=metric('admissions'),peak_active=metric('peak_active'),order_violations=metric('order_violations'),
    wait_total_ms=metric('wait_total_ms'),wait_max_ms=metric('wait_max_ms'),
    remaining=tonumber(redis.call('GET',stock) or '0'),reserved=redis.call('HLEN',reservations),
    active_limit=limit,mode=mode,name=name})
end
local function remove(id)
  local since = redis.call('ZSCORE',started,id)
  if since then
    redis.call('HINCRBY',metrics,'departures',1)
    redis.call('HINCRBY',metrics,'residence_total_ms',math.max(0,now-tonumber(since)))
  end
  redis.call('ZREM',active,id)
  redis.call('ZREM',started,id)
  redis.call('HDEL',generation,id)
end
local function expired(id)
  local beat = redis.call('ZSCORE',active,id)
  local since = redis.call('ZSCORE',started,id)
  return beat and (tonumber(beat) <= now-timeout or not since or tonumber(since) <= now-maxstay)
end
local wasExpired = false
if user ~= '' and expired(user) then
  remove(user)
  wasExpired = true
end
if action == 'heartbeat' and redis.call('ZSCORE',active,user) then
  redis.call('ZADD',active,now,user)
end
local function cleanup()
  -- Two indexes avoid scanning all admissions for either expiry condition.
  local stale = redis.call('ZRANGEBYSCORE',active,'-inf',now-timeout,'LIMIT',0,1000)
  local overstay = redis.call('ZRANGEBYSCORE',started,'-inf',now-maxstay,'LIMIT',0,1000)
  for _,id in ipairs(stale) do remove(id) end
  for _,id in ipairs(overstay) do remove(id) end
end
-- Admits up to max users from the front of the queue without exceeding the limit.
local function admit(max)
  local room = math.min(max,limit-redis.call('ZCARD',active))
  local admitted = {}
  if room <= 0 then return admitted end
  local rows = redis.call('ZRANGE',waiting,0,room-1,'WITHSCORES')
  for i=1,#rows,2 do
    local id, score = rows[i], tonumber(rows[i+1])
    redis.call('ZREM',waiting,id)
    redis.call('ZADD',active,now,id)
    redis.call('ZADD',started,now,id)
    redis.call('HSET',generation,id,string.format('%.0f',score))
    local wait = math.max(0,now-tonumber(redis.call('HGET',joined,id)))
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
  return admitted
end
-- tick is the worker's job: expire leases, then admit only in auto mode.
-- admit is the admin's "let the next N in" button and works in either mode.
if action == 'tick' or action == 'admit' then
  cleanup()
  local admitted = {}
  if action == 'admit' then
    admitted = admit(math.min(tonumber(ARGV[7]),1000))
  elseif mode == 'auto' then
    admitted = admit(1000)
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
if action == 'reserve' then
  local previous = redis.call('HGET',reservations,user)
  if previous then
    local old = cjson.decode(previous)
    if old.generation == ARGV[7] then return previous end
    return redis.error_reply('ALREADY_RESERVED')
  end
  if not redis.call('ZSCORE',active,user) or redis.call('HGET',generation,user) ~= ARGV[7] then
    return redis.error_reply('NOT_ADMITTED')
  end
  local remaining = tonumber(redis.call('GET',stock) or '0')
  if remaining <= 0 then return redis.error_reply('SOLD_OUT') end
  local result = cjson.encode({reservation_id=ARGV[8],generation=ARGV[7]})
  redis.call('DECR',stock)
  redis.call('HSET',reservations,user,result)
  remove(user)
  return result
end
local function encode(v) v.name = name; v.mode = mode; return cjson.encode(v) end
local function status()
  local reservation = redis.call('HGET',reservations,user)
  if reservation then
    return encode({status='completed',position=0,ahead=0,reservation_id=cjson.decode(reservation).reservation_id})
  end
  if redis.call('ZSCORE',active,user) then
    return encode({status='admitted',position=0,ahead=0,
      generation=redis.call('HGET',generation,user),
      expires_at=tonumber(redis.call('ZSCORE',started,user))+maxstay})
  end
  local rank = redis.call('ZRANK', waiting, user)
  if rank then
    local eta = cjson.null
    local departed = tonumber(redis.call('HGET',metrics,'departures') or '0')
    if departed > 0 and mode == 'auto' then
      local dwell = tonumber(redis.call('HGET',metrics,'residence_total_ms')) / departed
      eta = math.ceil((rank+1) * dwell / limit / 1000)
    end
    return encode({status='waiting', position=rank+1, ahead=rank,
      ticket=string.format('%.0f',tonumber(redis.call('ZSCORE',waiting,user))), estimated_wait_seconds=eta})
  end
  return encode({status=wasExpired and 'expired' or 'absent',position=0,ahead=0})
end
if action == 'join' then
  if not redis.call('HGET',reservations,user) and not redis.call('ZSCORE',waiting,user) and not redis.call('ZSCORE',active,user) then
    -- Logical microsecond score: INCR also orders ties and clock regressions.
    local s = math.max(redis.call('INCR',sequence), now * 1000)
    redis.call('SET',sequence,string.format('%.0f',s))
    redis.call('ZADD',waiting,s,user)
    redis.call('HSET',joined,user,now)
  end
end
return status()
