return {
  name = "write_file",
  description = "Create or overwrite a file in the workspace with the given content. Parent directories are created automatically.",
  parameters = {
    type = "object",
    properties = {
      path    = { type = "string", description = "File path relative to the workspace root" },
      content = { type = "string", description = "Full file content" },
    },
    required = { "path", "content" },
  },
  run = function(args)
    local ok, err = fs.write(args.path, args.content or "")
    if not ok then return nil, err end
    return string.format("wrote %d bytes to %s", #(args.content or ""), args.path)
  end,
}
