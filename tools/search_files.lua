return {
  name = "search_files",
  description = "Search for a literal string in workspace files. Returns path:line: text matches. Optionally filter file names by a substring (e.g. '.go').",
  parameters = {
    type = "object",
    properties = {
      query  = { type = "string", description = "Literal text to find (case-sensitive)" },
      path   = { type = "string", description = "Directory to search (default '.')" },
      filter = { type = "string", description = "Only search files whose path contains this" },
      max_results = { type = "integer", description = "Default 100" },
    },
    required = { "query" },
  },
  run = function(args)
    if not args.query or args.query == "" then return nil, "query is required" end
    local maxr = tonumber(args.max_results) or 100
    local files = fs.walk(args.path or ".", 3000)
    local out = {}
    for _, f in ipairs(files) do
      if not args.filter or f:find(args.filter, 1, true) then
        local data = fs.read(f)
        if data and not data:find("\0", 1, true) then
          local n = 0
          for line in (data .. "\n"):gmatch("(.-)\r?\n") do
            n = n + 1
            if line:find(args.query, 1, true) then
              out[#out + 1] = string.format("%s:%d: %s", f, n, line:sub(1, 200))
              if #out >= maxr then
                out[#out + 1] = "... [result limit reached]"
                return table.concat(out, "\n")
              end
            end
          end
        end
      end
    end
    if #out == 0 then return "no matches" end
    return table.concat(out, "\n")
  end,
}
