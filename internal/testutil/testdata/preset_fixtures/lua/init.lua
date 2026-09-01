local M = {}

local DEFAULT_CAPACITY = 64
local entries = {}

function M.insert(id, kind)
  if #entries >= DEFAULT_CAPACITY then
    return false
  end
  entries[id] = {id = id, kind = kind}
  return true
end

function M.lookup(id)
  return entries[id]
end

function M.count()
  local n = 0
  for _ in pairs(entries) do
    n = n + 1
  end
  return n
end

function M.kinds()
  local kinds = {"file", "directory", "symlink"}
  for i, k in ipairs(kinds) do
    print(i, k)
  end
end

function M.classify(id)
  if id == nil then
    return "unknown"
  end

  if string.sub(id, 1, 1) == "/" then
    return "absolute"
  else
    return "relative"
  end
end

function M.iterate(n)
  local i = 0
  while i < n do
    i = i + 1
  end
  return i
end

return M
