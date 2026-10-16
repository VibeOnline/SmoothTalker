return {
  name = "read_file",
  description = "Read a text file from the workspace. Returns lines prefixed with line numbers. Use offset/limit for large files.",
  parameters = {
    type = "object",
    properties = {
      path   = { type = "string",  description = "File path relative to the workspace root" },
      offset = { type = "integer", description = "First line to return, 1-based (default 1)" },
      limit  = { type = "integer", description = "Maximum number of lines (default 400)" },
    },
    required = { "path" },
  },
  run = function(args)
    local data, err = fs.read(args.path)
    if not data then return nil, err end
    if data:find("\0", 1, true) then return nil, "binary file" end

    local offset = math.max(1, math.floor(tonumber(args.offset) or 1))
    local limit  = math.max(1, math.floor(tonumber(args.limit) or 400))
    if data:sub(-1) ~= "\n" then data = data .. "\n" end

    local out, n = {}, 0
    for line in data:gmatch("(.-)\r?\n") do
      n = n + 1
      if n >= offset and n < offset + limit then
        out[#out + 1] = string.format("%6d\t%s", n, line)
      end
    end
    if #out == 0 then return string.format("(no lines in range; file has %d lines)", n) end
    local last = offset + #out - 1
    if last < n then
      out[#out + 1] = string.format("... [%d more lines, use offset=%d]", n - last, last + 1)
    end
    return table.concat(out, "\n")
  end,
}
