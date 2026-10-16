package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

const maxToolSourceBytes = 256 << 10

type toolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	File        string         `json:"file"`
	Loaded      bool           `json:"loaded"`
}

// compileTool compiles source and evaluates it in a sandbox to extract the
// tool definition. It does not register the tool.
func (r *luaRegistry) compileTool(file, src string) (*luaTool, error) {
	chunk, err := parse.Parse(strings.NewReader(src), file)
	if err != nil {
		return nil, fmt.Errorf("syntax error: %w", err)
	}
	proto, err := lua.Compile(chunk, file)
	if err != nil {
		return nil, fmt.Errorf("compile error: %w", err)
	}

	_, cancel := context.WithTimeout(context.Background(), luaLoadTimeout)
	defer cancel()
	L := r.newState() // restricted LState builder used by Call
	defer L.Close()

	L.Push(L.NewFunctionFromProto(proto))
	if err := L.PCall(0, 1, nil); err != nil {
		return nil, fmt.Errorf("runtime error: %w", err)
	}
	def, ok := L.Get(-1).(*lua.LTable)
	if !ok {
		return nil, errors.New("script must return a table")
	}

	name := lua.LVAsString(def.RawGetString("name"))
	if !luaToolNameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid tool name %q (allowed: A-Z a-z 0-9 _ -, max 64)", name)
	}
	if _, ok := def.RawGetString("run").(*lua.LFunction); !ok {
		return nil, errors.New("tool must define run(args)")
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

// invalidate forces the next Has/Definitions/Call to rescan the directory.
func (r *luaRegistry) invalidate() {
	r.mu.Lock()
	r.lastCheck = time.Time{}
	r.stamp = ""
	r.mu.Unlock()
}

func (r *luaRegistry) toolPath(name string) string {
	return filepath.Join(r.dir, name+".lua")
}

// ---------- HTTP ----------

func registerToolAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/api/tools", toolsList)
	mux.HandleFunc("GET /ui/api/tools/{name}", toolsGet)
	mux.HandleFunc("PUT /ui/api/tools/{name}", sameOrigin(toolsPut))
	mux.HandleFunc("DELETE /ui/api/tools/{name}", sameOrigin(toolsDelete))
	mux.HandleFunc("POST /ui/api/tools/validate", sameOrigin(toolsValidate))
	mux.HandleFunc("POST /ui/api/tools/{name}/test", sameOrigin(toolsTest))
}

func toolsList(w http.ResponseWriter, r *http.Request) {
	if !toolsEnabled(w) {
		return
	}
	defs := map[string]map[string]any{}
	for _, d := range LuaTools.Definitions() {
		fn := d["function"].(map[string]any)
		defs[fn["name"].(string)] = fn
	}

	entries, _ := os.ReadDir(LuaTools.dir)
	out := []toolInfo{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".lua")
		info := toolInfo{Name: name, File: e.Name()}
		if fn, ok := defs[name]; ok {
			info.Loaded = true
			info.Description, _ = fn["description"].(string)
			info.Parameters, _ = fn["parameters"].(map[string]any)
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func toolsGet(w http.ResponseWriter, r *http.Request) {
	name, ok := toolName(w, r)
	if !ok {
		return
	}
	src, err := os.ReadFile(LuaTools.toolPath(name))
	if errors.Is(err, os.ErrNotExist) {
		writeErr(w, http.StatusNotFound, "tool not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "source": string(src)})
}

type sourceBody struct {
	Source string `json:"source"`
}

func readSource(w http.ResponseWriter, r *http.Request) (string, bool) {
	var b sourceBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxToolSourceBytes)).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return "", false
	}
	return b.Source, true
}

func toolsValidate(w http.ResponseWriter, r *http.Request) {
	if !toolsEnabled(w) {
		return
	}
	src, ok := readSource(w, r)
	if !ok {
		return
	}
	t, err := LuaTools.compileTool("draft.lua", src)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toolInfo{Name: t.Name, Description: t.Description, Parameters: t.Params, Loaded: true})
}

func toolsPut(w http.ResponseWriter, r *http.Request) {
	name, ok := toolName(w, r)
	if !ok {
		return
	}
	src, ok := readSource(w, r)
	if !ok {
		return
	}
	t, err := LuaTools.compileTool(name+".lua", src)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if t.Name != name {
		writeErr(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("script declares name %q but is being saved as %q", t.Name, name))
		return
	}

	if err := os.MkdirAll(LuaTools.dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tmp, err := os.CreateTemp(LuaTools.dir, "."+name+"-*.tmp")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, werr := tmp.WriteString(src)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		writeErr(w, http.StatusInternalServerError, "write failed")
		return
	}
	if err := os.Rename(tmp.Name(), LuaTools.toolPath(name)); err != nil {
		os.Remove(tmp.Name())
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	LuaTools.invalidate()
	writeJSON(w, http.StatusOK, toolInfo{Name: t.Name, Description: t.Description, Parameters: t.Params, File: name + ".lua", Loaded: true})
}

func toolsDelete(w http.ResponseWriter, r *http.Request) {
	name, ok := toolName(w, r)
	if !ok {
		return
	}
	if err := os.Remove(LuaTools.toolPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	LuaTools.invalidate()
	w.WriteHeader(http.StatusNoContent)
}

func toolsTest(w http.ResponseWriter, r *http.Request) {
	name, ok := toolName(w, r)
	if !ok {
		return
	}
	var b struct {
		Args map[string]any `json:"args"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if !LuaTools.Has(name) {
		writeErr(w, http.StatusNotFound, "tool not loaded (save it first)")
		return
	}
	start := time.Now()
	result := LuaTools.Call(r.Context(), name, b.Args)
	writeJSON(w, http.StatusOK, map[string]any{
		"result":     result,
		"durationMs": time.Since(start).Milliseconds(),
	})
}

// ---------- helpers ----------

func toolsEnabled(w http.ResponseWriter) bool {
	if LuaTools == nil {
		writeErr(w, http.StatusServiceUnavailable, "lua tools are disabled (workspace not available)")
		return false
	}
	return true
}

func toolName(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !toolsEnabled(w) {
		return "", false
	}
	name := r.PathValue("name")
	if !luaToolNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid tool name")
		return "", false
	}
	return name, true
}

// sameOrigin blocks cross-site requests (CSRF) from other web pages.
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Host != r.Host {
				writeErr(w, http.StatusForbidden, "cross-origin request rejected")
				return
			}
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
