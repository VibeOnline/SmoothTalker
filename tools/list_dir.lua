return {
  name = "list_dir",
  description = "List files and directories at a path in the workspace. Directories end with '/'.",
  parameters = {
    type = "object",
    properties = {
      path = { type = "string", description = "Directory relative to the workspace root (default '.')" },
    },
  },
  run = function(args)
    local entries, err = fs.list(args.path or ".")
    if not entries then return nil, err end
    table.sort(entries, function(a, b)
      if a.is_dir ~= b.is_dir then return a.is_dir end
      return a.name < b.name
    end)
    local out = {}
    for _, e in ipairs(entries) do
      if e.is_dir then
        out[#out + 1] = e.name .. "/"
      else
        out[#out + 1] = string.format("%s  (%d bytes)", e.name, e.size or 0)
      end
    end
    if #out == 0 then return "(empty directory)" end
    return table.concat(out, "\n")
  end,
}
