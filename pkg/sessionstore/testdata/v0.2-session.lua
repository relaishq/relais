
local op, owner, epoch, ttl = ARGV[1], ARGV[2], ARGV[3], tonumber(ARGV[4])
local retention = tonumber(ARGV[5])
local current = redis.call('HMGET', KEYS[1], 'worker', 'epoch', 'expires')
local function reply() return {1, current[1], current[2], current[3]} end
local function matches() return current[1] == owner and current[2] == epoch end
local function greater(a,b) return not b or #a>#b or (#a==#b and a>b) end
local function newer(candidate) return greater(candidate,redis.call('GET',KEYS[4])) end
local function now()
 local t=redis.call('TIME')
 return tonumber(t[1])*1000 + math.floor(tonumber(t[2])/1000)
end
local function retain(candidate)
 redis.call('SET',KEYS[4],candidate)
 redis.call('PEXPIREAT',KEYS[4],math.max(now(),tonumber(current[3]) or 0)+retention)
end
local function write(worker, nextEpoch)
 redis.call('DEL',KEYS[6])
 local expires=string.format('%.0f',now()+ttl)
 redis.call('HSET',KEYS[1],'worker',worker,'epoch',nextEpoch,'expires',expires)
 redis.call('PEXPIREAT',KEYS[1],expires)
 redis.call('PEXPIREAT',KEYS[2],expires)
 redis.call('SET',KEYS[3],worker .. '/' .. nextEpoch)
 redis.call('PEXPIREAT',KEYS[3],tonumber(expires)+retention)
 redis.call('PEXPIREAT',KEYS[4],tonumber(expires)+retention)
 current={worker,nextEpoch,expires}
 return reply()
end
if op=='settle' or op=='indexed' then
 if matches() then return reply() end
 -- A young prepublication may still commit. Never fence or prune it.
 if op=='indexed' and newer(epoch) and now()<tonumber(ARGV[6])+tonumber(ARGV[7]) then return {3} end
 if op=='indexed' and not current[1] and redis.call('GET',KEYS[3])==owner .. '/' .. epoch then
  redis.call('DEL',KEYS[2],KEYS[3],KEYS[5])
 end
 -- This also settles an indexed prepublication racing its transition. Once
 -- pruned, that candidate is fenced, never a live lease missing its index.
 if newer(epoch) then retain(epoch) end
 return {2}
elseif op=='claim' then
 if redis.call('EXISTS',KEYS[3])==1 then return {-1} end
 if not newer(epoch) then return {-3} end
 retain(epoch)
 redis.call('DEL',KEYS[2],KEYS[5])
 return write(owner,epoch)
elseif op=='get' or op=='state' or op=='checkpoint' then
 if not current[1] then
  redis.call('DEL',KEYS[2],KEYS[3],KEYS[5])
  if op=='get' then
   local ended=redis.call('HMGET',KEYS[6],'worker','epoch','expires')
   if ended[1] then return {4,ended[1],ended[2],ended[3]} end
  end
  return {0}
 end
 if op=='get' then return reply() end
 if op=='checkpoint' then
  local stored=redis.call('HGET',KEYS[1],'checkpoint_at')
  if not stored then return {0} end
  local digest=redis.call('HGET',KEYS[1],'state_digest')
  local matched=false
  for _,candidate in ipairs(cjson.decode(ARGV[6])) do
   if candidate==digest then matched=true; break end
  end
  if not matched then return {-4} end
  return {1,stored,string.format('%.0f',now())}
 end
 local blob=redis.call('GET',KEYS[2])
 if not blob then return {0} end
 return {1,blob,redis.call('HGET',KEYS[1],'state_seq') or ''}
elseif op=='release' then
 if matches() then
  local expires=string.format('%.0f',now()+tonumber(ARGV[6]))
  redis.call('HSET',KEYS[6],'worker',owner,'epoch',epoch,'expires',expires)
  redis.call('PEXPIREAT',KEYS[6],expires)
 end
 if matches() or (not current[1] and redis.call('GET',KEYS[3])==owner .. '/' .. epoch) then
  redis.call('DEL',KEYS[1],KEYS[2],KEYS[3],KEYS[5])
 end
 return {1}
end
if not matches() then return {-2} end
if op=='renew' then return write(owner,epoch)
elseif op=='transfer' then
 if not newer(ARGV[7]) then return {-3} end
 retain(ARGV[7])
 redis.call('DEL',KEYS[5])
 return write(ARGV[6],ARGV[7])
elseif op=='sequence' then
 redis.call('HINCRBY',KEYS[1],'next_seq',1)
 return {1,redis.call('HGET',KEYS[1],'next_seq')}
elseif op=='put' then
 local previous=redis.call('HGET',KEYS[1],'state_seq')
 if not greater(ARGV[7],previous) then return {-4} end
 redis.call('SET',KEYS[2],ARGV[6])
 redis.call('HSET',KEYS[1],'state_seq',ARGV[7],'checkpoint_at',string.format('%.0f',now()),'state_digest',ARGV[8])
 redis.call('PEXPIREAT',KEYS[2],current[3])
 return {1}
end
return redis.error_reply('unknown session operation')
