package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

const (
	luaToolTimeout    = 10 * time.Second
	luaLoadTimeout    = 2 * time.Second
	luaRefreshEvery   = 1 * time.Second
	luaMaxReadBytes   = 2 << 20   // per fs.read
	luaMaxWriteBytes  = 4 << 20   // per fs.write
	luaMaxResultBytes = 256 << 10 // returned to the model
	luaMaxWalkEntries = 5000
)

var luaToolNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type luaTool struct {
	Name        string
	Description string
	Params      map[string]any
	File        string
	proto       *lua.FunctionProto
}

type luaRegistry struct {
	mu        sync.RWMutex
	dir       string   // where *.lua tool scripts live
	root      *os.Root // workspace the scripts may touch
	readOnly  bool
	tools     map[string]*luaTool
	stamp     string
	lastCheck time.Time
}

var LuaTools *luaRegistry

// InitLuaTools sets up the registry. Configure with env vars:
//
//	LUA_TOOLS_DIR      (default "./tools")
//	LUA_WORKSPACE      (default current directory)
//	LUA_TOOLS_READONLY ("1" disables fs.write/mkdir/remove)
func InitLuaTools() {
	dir := envOr("LUA_TOOLS_DIR", "tools")
	ws := envOr("LUA_WORKSPACE", ".")
	root, err := os.OpenRoot(ws)
	if err != nil {
		log.Printf("lua tools disabled: cannot open workspace %q: %v", ws, err)
		return
	}
	LuaTools = &luaRegistry{
		dir:      dir,
		root:     root,
		readOnly: os.Getenv("LUA_TOOLS_READONLY") == "1",
		tools:    map[string]*luaTool{},
	}
	LuaTools.refresh(true)
	log.Printf("lua tools: dir=%s workspace=%s readonly=%v loaded=%d",
		dir, root.Name(), LuaTools.readOnly, len(LuaTools.tools))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------- public API used by main.go ----------

func (r *luaRegistry) Has(name string) bool {
	if r == nil {
		return false
	}
	r.refresh(false)
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}

// Definitions returns OpenAI-style tool definitions.
func (r *luaRegistry) Definitions() []map[string]any {
	if r == nil {
		return nil
	}
	r.refresh(false)
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Params,
			},
		})
	}
	return out
}

// Call runs a tool and returns a string result for the model (errors included as text).
func (r *luaRegistry) Call(ctx context.Context, name string, args map[string]any) string {
	r.mu.RLock()
	t := r.tools[name]
	r.mu.RUnlock()
	if t == nil {
		return "error: unknown tool " + name
	}

	ctx, cancel := context.WithTimeout(ctx, luaToolTimeout)
	defer cancel()
	L := r.newState()
	defer L.Close()
	L.SetContext(ctx)

	def, err := evalScript(L, t.proto)
	if err != nil {
		return "error: " + err.Error()
	}
	run, ok := def.RawGetString("run").(*lua.LFunction)
	if !ok {
		return "error: tool has no run function"
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := L.CallByParam(lua.P{Fn: run, NRet: 2, Protect: true}, toLua(L, args)); err != nil {
		if ctx.Err() != nil {
			return "error: tool timed out"
		}
		return "error: " + err.Error()
	}
	ret, errv := L.Get(-2), L.Get(-1)
	L.Pop(2)

	if ret == lua.LNil && errv != lua.LNil {
		return "error: " + errv.String()
	}
	var s string
	switch v := ret.(type) {
	case lua.LString:
		s = string(v)
	case *lua.LNilType:
		s = "ok"
	default:
		b, err := json.Marshal(fromLua(v, 0))
		if err != nil {
			return "error: encoding result: " + err.Error()
		}
		s = string(b)
	}
	if len(s) > luaMaxResultBytes {
		s = s[:luaMaxResultBytes] + "\n... [truncated]"
	}
	return s
}

// ---------- loading ----------

func (r *luaRegistry) refresh(force bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !force && time.Since(r.lastCheck) < luaRefreshEvery {
		return
	}
	r.lastCheck = time.Now()

	entries, err := os.ReadDir(r.dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("lua tools: reading %s: %v", r.dir, err)
		}
		r.tools, r.stamp = map[string]*luaTool{}, ""
		return
	}
	var sb strings.Builder
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "%s|%d|%d;", e.Name(), info.Size(), info.ModTime().UnixNano())
		files = append(files, path.Join(r.dir, e.Name()))
	}
	if sb.String() == r.stamp && !force {
		return
	}

	tools := map[string]*luaTool{}
	for _, f := range files {
		t, err := r.loadTool(f)
		if err != nil {
			log.Printf("lua tools: %s: %v", f, err)
			continue
		}
		if prev, dup := tools[t.Name]; dup {
			log.Printf("lua tools: %s: duplicate name %q (already in %s)", f, t.Name, prev.File)
			continue
		}
		tools[t.Name] = t
	}
	r.tools, r.stamp = tools, sb.String()
	log.Printf("lua tools: loaded %d tool(s)", len(tools))
}

func (r *luaRegistry) loadTool(file string) (*luaTool, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	chunk, err := parse.Parse(strings.NewReader(string(src)), file)
	if err != nil {
		return nil, err
	}
	proto, err := lua.Compile(chunk, file)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), luaLoadTimeout)
	defer cancel()
	L := r.newState()
	defer L.Close()
	L.SetContext(ctx)

	def, err := evalScript(L, proto)
	if err != nil {
		return nil, err
	}
	name := lua.LVAsString(def.RawGetString("name"))
	if !luaToolNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid or missing name %q", name)
	}
	if _, ok := def.RawGetString("run").(*lua.LFunction); !ok {
		return nil, errors.New("missing run function")
	}
	params, _ := fromLua(def.RawGetString("parameters"), 0).(map[string]any)
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return &luaTool{
		Name:        name,
		Description: lua.LVAsString(def.RawGetString("description")),
		Params:      params,
		File:        file,
		proto:       proto,
	}, nil
}

func evalScript(L *lua.LState, proto *lua.FunctionProto) (*lua.LTable, error) {
	L.Push(L.NewFunctionFromProto(proto))
	if err := L.PCall(0, 1, nil); err != nil {
		return nil, err
	}
	tbl, ok := L.Get(-1).(*lua.LTable)
	L.Pop(1)
	if !ok {
		return nil, errors.New("script must return a table")
	}
	return tbl, nil
}

// ---------- sandbox ----------

func (r *luaRegistry) newState() *lua.LState {
	L := lua.NewState(lua.Options{SkipOpenLibs: true, CallStackSize: 256})
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	for _, n := range []string{"dofile", "loadfile", "load", "loadstring", "require", "module", "collectgarbage"} {
		L.SetGlobal(n, lua.LNil)
	}
	L.SetGlobal("print", L.NewFunction(func(L *lua.LState) int {
		parts := make([]string, L.GetTop())
		for i := range parts {
			parts[i] = L.Get(i + 1).String()
		}
		log.Printf("[lua] %s", strings.Join(parts, "\t"))
		return 0
	}))
	L.SetGlobal("fs", r.fsModule(L))
	L.SetGlobal("json", jsonModule(L))
	return L
}

// cleanRel turns any user path into an io/fs-valid path relative to the workspace.
func cleanRel(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean("/" + p)[1:]
	if p == "" {
		p = "."
	}
	if !fs.ValidPath(p) {
		return "", fmt.Errorf("invalid path %q", p)
	}
	return p, nil
}

func luaFail(L *lua.LState, err error) int {
	L.Push(lua.LNil)
	L.Push(lua.LString(err.Error()))
	return 2
}

func (r *luaRegistry) fsModule(L *lua.LState) *lua.LTable {
	fsys := r.root.FS()
	mod := L.NewTable()

	writable := func(L *lua.LState) error {
		if r.readOnly {
			return errors.New("workspace is read-only")
		}
		return nil
	}

	fns := map[string]lua.LGFunction{
		// fs.read(path) -> string | nil, err
		"read": func(L *lua.LState) int {
			p, err := cleanRel(L.CheckString(1))
			if err != nil {
				return luaFail(L, err)
			}
			f, err := fsys.Open(p)
			if err != nil {
				return luaFail(L, err)
			}
			defer f.Close()
			if st, err := f.Stat(); err == nil && st.IsDir() {
				return luaFail(L, fmt.Errorf("%s is a directory", p))
			}
			data, err := io.ReadAll(io.LimitReader(f, luaMaxReadBytes+1))
			if err != nil {
				return luaFail(L, err)
			}
			if len(data) > luaMaxReadBytes {
				return luaFail(L, fmt.Errorf("%s is larger than %d bytes", p, luaMaxReadBytes))
			}
			L.Push(lua.LString(data))
			return 1
		},
		// fs.write(path, content [, append]) -> true | nil, err
		"write": func(L *lua.LState) int {
			if err := writable(L); err != nil {
				return luaFail(L, err)
			}
			p, err := cleanRel(L.CheckString(1))
			if err != nil {
				return luaFail(L, err)
			}
			content := L.CheckString(2)
			if len(content) > luaMaxWriteBytes {
				return luaFail(L, errors.New("content too large"))
			}
			if err := r.mkdirAll(path.Dir(p)); err != nil {
				return luaFail(L, err)
			}
			flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			if L.OptBool(3, false) {
				flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
			}
			f, err := r.root.OpenFile(p, flags, 0o644)
			if err != nil {
				return luaFail(L, err)
			}
			_, werr := f.WriteString(content)
			cerr := f.Close()
			if err := errors.Join(werr, cerr); err != nil {
				return luaFail(L, err)
			}
			L.Push(lua.LTrue)
			return 1
		},
		// fs.list(path) -> { {name=, is_dir=, size=}, ... } | nil, err
		"list": func(L *lua.LState) int {
			p, err := cleanRel(L.OptString(1, "."))
			if err != nil {
				return luaFail(L, err)
			}
			entries, err := fs.ReadDir(fsys, p)
			if err != nil {
				return luaFail(L, err)
			}
			out := L.NewTable()
			for _, e := range entries {
				t := L.NewTable()
				t.RawSetString("name", lua.LString(e.Name()))
				t.RawSetString("is_dir", lua.LBool(e.IsDir()))
				if info, err := e.Info(); err == nil {
					t.RawSetString("size", lua.LNumber(info.Size()))
				}
				out.Append(t)
			}
			L.Push(out)
			return 1
		},
		// fs.stat(path) -> {name, size, is_dir, mod_time} | nil, err
		"stat": func(L *lua.LState) int {
			p, err := cleanRel(L.CheckString(1))
			if err != nil {
				return luaFail(L, err)
			}
			info, err := fs.Stat(fsys, p)
			if err != nil {
				return luaFail(L, err)
			}
			t := L.NewTable()
			t.RawSetString("name", lua.LString(info.Name()))
			t.RawSetString("size", lua.LNumber(info.Size()))
			t.RawSetString("is_dir", lua.LBool(info.IsDir()))
			t.RawSetString("mod_time", lua.LNumber(info.ModTime().Unix()))
			L.Push(t)
			return 1
		},
		"exists": func(L *lua.LState) int {
			p, err := cleanRel(L.CheckString(1))
			if err != nil {
				L.Push(lua.LFalse)
				return 1
			}
			_, err = fs.Stat(fsys, p)
			L.Push(lua.LBool(err == nil))
			return 1
		},
		"mkdir": func(L *lua.LState) int {
			if err := writable(L); err != nil {
				return luaFail(L, err)
			}
			p, err := cleanRel(L.CheckString(1))
			if err != nil {
				return luaFail(L, err)
			}
			if err := r.mkdirAll(p); err != nil {
				return luaFail(L, err)
			}
			L.Push(lua.LTrue)
			return 1
		},
		"remove": func(L *lua.LState) int {
			if err := writable(L); err != nil {
				return luaFail(L, err)
			}
			p, err := cleanRel(L.CheckString(1))
			if err != nil || p == "." {
				return luaFail(L, errors.New("refusing to remove workspace root"))
			}
			if err := r.root.Remove(p); err != nil {
				return luaFail(L, err)
			}
			L.Push(lua.LTrue)
			return 1
		},
		// fs.walk(path [, max]) -> { "rel/file", ... }  (skips dot-dirs and node_modules)
		"walk": func(L *lua.LState) int {
			p, err := cleanRel(L.OptString(1, "."))
			if err != nil {
				return luaFail(L, err)
			}
			max := L.OptInt(2, 1000)
			if max <= 0 || max > luaMaxWalkEntries {
				max = luaMaxWalkEntries
			}
			out := L.NewTable()
			n := 0
			_ = fs.WalkDir(fsys, p, func(fp string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if L.Context() != nil && L.Context().Err() != nil {
					return fs.SkipAll
				}
				if d.IsDir() {
					if fp != p && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
						return fs.SkipDir
					}
					return nil
				}
				out.Append(lua.LString(fp))
				n++
				if n >= max {
					return fs.SkipAll
				}
				return nil
			})
			L.Push(out)
			return 1
		},
	}
	L.SetFuncs(mod, fns)
	mod.RawSetString("readonly", lua.LBool(r.readOnly))
	return mod
}

func (r *luaRegistry) mkdirAll(p string) error {
	if p == "." || p == "" {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(p, "/") {
		cur = path.Join(cur, part)
		if err := r.root.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}

func jsonModule(L *lua.LState) *lua.LTable {
	mod := L.NewTable()
	L.SetFuncs(mod, map[string]lua.LGFunction{
		"encode": func(L *lua.LState) int {
			b, err := json.Marshal(fromLua(L.Get(1), 0))
			if err != nil {
				return luaFail(L, err)
			}
			L.Push(lua.LString(b))
			return 1
		},
		"decode": func(L *lua.LState) int {
			var v any
			if err := json.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
				return luaFail(L, err)
			}
			L.Push(toLua(L, v))
			return 1
		},
	})
	return mod
}

// ---------- Go <-> Lua conversion ----------

func toLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(x)
	case float64:
		return lua.LNumber(x)
	case int:
		return lua.LNumber(x)
	case string:
		return lua.LString(x)
	case []any:
		t := L.CreateTable(len(x), 0)
		for _, e := range x {
			t.Append(toLua(L, e))
		}
		return t
	case map[string]any:
		t := L.CreateTable(0, len(x))
		for k, e := range x {
			t.RawSetString(k, toLua(L, e))
		}
		return t
	default:
		return lua.LString(fmt.Sprint(x))
	}
}

// fromLua: tables with keys 1..n become arrays; anything else (including {}) becomes an object.
func fromLua(v lua.LValue, depth int) any {
	if depth > 64 {
		return nil
	}
	switch x := v.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(x)
	case lua.LNumber:
		return float64(x)
	case lua.LString:
		return string(x)
	case *lua.LTable:
		n := x.MaxN()
		count := 0
		x.ForEach(func(_, _ lua.LValue) { count++ })
		if n > 0 && n == count {
			arr := make([]any, 0, n)
			for i := 1; i <= n; i++ {
				arr = append(arr, fromLua(x.RawGetInt(i), depth+1))
			}
			return arr
		}
		m := map[string]any{}
		x.ForEach(func(k, val lua.LValue) {
			m[k.String()] = fromLua(val, depth+1)
		})
		return m
	default:
		return v.String()
	}
}

// ---------- tool-call plumbing ----------

func toolCallNameArgs(call map[string]any) (string, map[string]any) {
	fn, _ := call["function"].(map[string]any)
	name, _ := fn["name"].(string)
	switch a := fn["arguments"].(type) {
	case map[string]any: // Ollama native
		return name, a
	case string: // OpenAI: JSON string
		var m map[string]any
		if json.Unmarshal([]byte(a), &m) == nil {
			return name, m
		}
	}
	return name, map[string]any{}
}

// dispatchToolCall routes to Lua tools first, then MCP.
func dispatchToolCall(ctx context.Context, call map[string]any, opts chatOpts) string {
	name, args := toolCallNameArgs(call)
	if opts.luaTools && LuaTools.Has(name) {
		return LuaTools.Call(ctx, name, args)
	}
	result, err := handleMCPCall(ctx, call)
	if err != nil && result == "" {
		return "error: " + err.Error()
	}
	return result
}
