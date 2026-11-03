// Package skills loads and runs small Lua tools that the model can author at
// runtime. Lua is used as a narrow extension layer: skills get a deliberately
// small host API, never raw os/io access, and command execution still goes
// through aish's safety classifier.
package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

const ToolPrefix = "lua_"

var (
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	reserved = map[string]bool{
		"run_command":  true,
		"find_package": true,
		"propose_lua":  true,
	}
)

// ExecResult is one command run on behalf of a Lua skill.
type ExecResult struct {
	Code   int
	Stdout string
	Stderr string
}

// Host is the narrow bridge from Lua back into aish.
type Host interface {
	Exec(ctx context.Context, argv []string) (ExecResult, error)
	Print(text string)
	Log(text string)
	Cwd() string
	Env(name string) string
}

// Info describes a loaded skill for :skills / :skill show.
type Info struct {
	Name        string
	Description string
	Tool        string
	Path        string
	Persistent  bool
	Error       string
}

type fileState struct {
	mod  int64
	size int64
}

type skill struct {
	name        string
	description string
	params      map[string]any
	source      string
	path        string
	persistent  bool
	createdBy   string

	L       *lua.LState
	fn      *lua.LFunction
	host    Host
	timeout time.Duration
	mu      sync.Mutex
}

func (s *skill) toolName() string { return ToolPrefix + s.name }

func (s *skill) close() {
	if s.L != nil {
		s.L.Close()
		s.L = nil
	}
}

// Manager owns the session skill registry and the on-disk skills directory.
type Manager struct {
	dir       string
	timeout   time.Duration
	maxSkills int
	maxBytes  int
	host      Host

	mu     sync.RWMutex
	skills map[string]*skill
	files  map[string]fileState
	errs   map[string]string
}

func NewManager(dir string, timeout time.Duration, maxSkills, maxBytes int, host Host) *Manager {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if maxSkills <= 0 {
		maxSkills = 8
	}
	if maxBytes <= 0 {
		maxBytes = 64 * 1024
	}
	return &Manager{
		dir:       dir,
		timeout:   timeout,
		maxSkills: maxSkills,
		maxBytes:  maxBytes,
		host:      host,
		skills:    map[string]*skill{},
		files:     map[string]fileState{},
		errs:      map[string]string{},
	}
}

func (m *Manager) Dir() string { return m.dir }

// LoadDir loads new/changed *.lua files and removes deleted ones. It returns
// only the errors encountered during this reload; unchanged files are ignored.
func (m *Manager) LoadDir() []error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []error{err}
	}

	seen := map[string]fileState{}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".lua")
		st := fileState{mod: info.ModTime().UnixNano(), size: info.Size()}
		seen[name] = st
		if old, ok := m.files[name]; ok && old == st {
			continue
		}
		path := filepath.Join(m.dir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			m.errs[name] = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		s, err := m.loadLocked(name, string(src), path, true, "disk")
		if err != nil {
			m.errs[name] = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		if oldS := m.skills[name]; oldS != nil {
			oldS.close()
		}
		m.skills[name] = s
		delete(m.errs, name)
	}

	for name, s := range m.skills {
		if !s.persistent {
			continue
		}
		if _, ok := seen[name]; !ok {
			delete(m.skills, name)
			delete(m.errs, name)
			s.close()
		}
	}
	m.files = seen
	return errs
}

// Propose compiles a model-authored skill and registers it for this session.
// The source is kept so the user can persist it later with Save.
func (m *Manager) Propose(name, description, code string, params map[string]any) (*Info, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid skill name %q (use lowercase letters, digits, underscore; max 32)", name)
	}
	if reserved[name] {
		return nil, fmt.Errorf("%q is a reserved tool name", name)
	}
	if strings.TrimSpace(code) == "" {
		return nil, fmt.Errorf("empty Lua code")
	}
	if len(code) > m.maxBytes {
		return nil, fmt.Errorf("Lua code too large (max %d bytes)", m.maxBytes)
	}
	params = normalizeParams(params)
	description = strings.TrimSpace(description)
	if description == "" {
		description = "Model-authored Lua tool " + name
	}
	if len(description) > 400 {
		description = description[:400]
	}
	src := buildSource(name, description, params, code)
	if len(src) > m.maxBytes {
		return nil, fmt.Errorf("generated skill too large (max %d bytes)", m.maxBytes)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.skills[name]; ok {
		return nil, fmt.Errorf("skill %q already exists", name)
	}
	if len(m.skills) >= m.maxSkills {
		return nil, fmt.Errorf("session skill limit reached (%d)", m.maxSkills)
	}
	s, err := m.loadLocked(name, src, "", false, "model")
	if err != nil {
		return nil, err
	}
	m.skills[name] = s
	return infoOf(s, ""), nil
}

func (m *Manager) loadLocked(nameHint, source, path string, persistent bool, createdBy string) (*skill, error) {
	if len(source) > m.maxBytes {
		return nil, fmt.Errorf("source too large (max %d bytes)", m.maxBytes)
	}
	s := &skill{
		source:     source,
		path:       path,
		persistent: persistent,
		createdBy:  createdBy,
		host:       m.host,
		timeout:    m.timeout,
	}
	L, err := newSandbox(s)
	if err != nil {
		return nil, err
	}
	s.L = L

	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	L.SetContext(ctx)
	defer L.RemoveContext()

	chunk, err := L.LoadString(source)
	if err != nil {
		s.close()
		return nil, fmt.Errorf("compile: %w", err)
	}
	if err := L.CallByParam(lua.P{Fn: chunk, NRet: 1, Protect: true}); err != nil {
		s.close()
		return nil, fmt.Errorf("load: %w", err)
	}
	ret := L.Get(-1)
	L.Pop(1)
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		s.close()
		return nil, fmt.Errorf("skill must return a table")
	}

	name := strings.TrimSpace(luaString(tbl.RawGetString("name")))
	if name == "" {
		name = nameHint
	}
	if !nameRE.MatchString(name) {
		s.close()
		return nil, fmt.Errorf("invalid skill name %q", name)
	}
	if nameHint != "" && name != nameHint {
		s.close()
		return nil, fmt.Errorf("skill name %q does not match filename %q", name, nameHint)
	}
	if reserved[name] {
		s.close()
		return nil, fmt.Errorf("%q is a reserved tool name", name)
	}
	if v := tbl.RawGetString("api"); v != lua.LNil {
		if n, ok := v.(lua.LNumber); !ok || int(n) != 1 {
			s.close()
			return nil, fmt.Errorf("unsupported skill api version %v", v)
		}
	}
	desc := strings.TrimSpace(luaString(tbl.RawGetString("description")))
	if desc == "" {
		desc = "Lua skill " + name
	}
	params, err := luaParams(tbl.RawGetString("parameters"))
	if err != nil {
		s.close()
		return nil, err
	}
	params = normalizeParams(params)
	run, ok := tbl.RawGetString("run").(*lua.LFunction)
	if !ok {
		s.close()
		return nil, fmt.Errorf("skill %q is missing a run(args) function", name)
	}

	s.name = name
	s.description = desc
	s.params = params
	s.fn = run
	installAPI(L, s)
	return s, nil
}

func newSandbox(s *skill) (*lua.LState, error) {
	L := lua.NewState(lua.Options{
		SkipOpenLibs:     true,
		CallStackSize:    256,
		RegistrySize:     1024,
		RegistryMaxSize:  4096,
		RegistryGrowStep: 64,
	})
	lua.OpenBase(L)
	lua.OpenTable(L)
	lua.OpenString(L)
	lua.OpenMath(L)

	for _, name := range []string{
		"dofile", "load", "loadfile", "loadstring", "require", "module",
		"collectgarbage", "print", "_printregs", "newproxy", "getfenv", "setfenv",
	} {
		L.SetGlobal(name, lua.LNil)
	}
	return L, nil
}

func installAPI(L *lua.LState, s *skill) {
	api := L.NewTable()
	L.SetFuncs(api, map[string]lua.LGFunction{
		"exec":        s.luaExec,
		"print":       s.luaPrint,
		"log":         s.luaLog,
		"cwd":         s.luaCwd,
		"env_get":     s.luaEnvGet,
		"json_encode": s.luaJSONEncode,
		"json_decode": s.luaJSONDecode,
	})
	L.SetGlobal("aish", api)
}

func (s *skill) luaExec(L *lua.LState) int {
	tbl := L.CheckTable(1)
	argv := make([]string, 0, tbl.Len())
	for i := 1; i <= tbl.Len(); i++ {
		v, ok := tbl.RawGetInt(i).(lua.LString)
		if !ok {
			L.RaiseError("aish.exec: argv must contain only strings")
			return 0
		}
		argv = append(argv, string(v))
	}
	if len(argv) == 0 {
		L.RaiseError("aish.exec: argv must not be empty")
		return 0
	}
	ctx := L.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := s.host.Exec(ctx, argv)
	t := L.NewTable()
	if err != nil {
		t.RawSetString("code", lua.LNumber(-1))
		t.RawSetString("stdout", lua.LString(""))
		t.RawSetString("stderr", lua.LString(err.Error()))
	} else {
		t.RawSetString("code", lua.LNumber(res.Code))
		t.RawSetString("stdout", lua.LString(res.Stdout))
		t.RawSetString("stderr", lua.LString(res.Stderr))
	}
	L.Push(t)
	return 1
}

func (s *skill) luaPrint(L *lua.LState) int {
	s.host.Print(L.OptString(1, ""))
	return 0
}

func (s *skill) luaLog(L *lua.LState) int {
	s.host.Log(L.OptString(1, ""))
	return 0
}

func (s *skill) luaCwd(L *lua.LState) int {
	L.Push(lua.LString(s.host.Cwd()))
	return 1
}

func (s *skill) luaEnvGet(L *lua.LState) int {
	L.Push(lua.LString(s.host.Env(L.CheckString(1))))
	return 1
}

func (s *skill) luaJSONEncode(L *lua.LState) int {
	b, err := json.Marshal(fromLua(L.Get(1)))
	if err != nil {
		L.RaiseError("json_encode: %v", err)
		return 0
	}
	L.Push(lua.LString(string(b)))
	return 1
}

func (s *skill) luaJSONDecode(L *lua.LState) int {
	var v any
	if err := json.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
		L.RaiseError("json_decode: %v", err)
		return 0
	}
	L.Push(toLua(L, v))
	return 1
}

// Call runs a registered skill by its lua_* tool name.
func (m *Manager) Call(ctx context.Context, toolName string, args map[string]any) (string, error) {
	if !strings.HasPrefix(toolName, ToolPrefix) {
		return "", fmt.Errorf("not a Lua tool: %s", toolName)
	}
	name := strings.TrimPrefix(toolName, ToolPrefix)

	// Hold the read lock for the whole call so a reload cannot close the Lua
	// state underneath us.
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.skills[name]
	if s == nil {
		return "", fmt.Errorf("unknown Lua skill %q", name)
	}
	return s.call(ctx, args)
}

func (s *skill) call(ctx context.Context, args map[string]any) (out string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.L == nil {
		return "", fmt.Errorf("skill %q is closed", s.name)
	}
	L := s.L
	L.SetTop(0)
	defer func() {
		if r := recover(); r != nil {
			L.SetTop(0)
			out = ""
			err = fmt.Errorf("lua panic: %v", r)
		}
	}()

	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	L.SetContext(callCtx)
	defer L.RemoveContext()

	if args == nil {
		args = map[string]any{}
	}
	tbl := s.argsTable(L, args)
	if err := L.CallByParam(lua.P{Fn: s.fn, NRet: 1, Protect: true}, tbl); err != nil {
		L.SetTop(0)
		return "", err
	}
	ret := L.Get(-1)
	L.Pop(1)
	return resultString(ret)
}

// argsTable builds the args table and also exposes positional aliases in the
// schema's required order, so model-written `args[1]` still works.
func (s *skill) argsTable(L *lua.LState, args map[string]any) *lua.LTable {
	tbl := toLua(L, args).(*lua.LTable)
	seen := map[string]bool{}
	var order []string
	if req, ok := s.params["required"].([]any); ok {
		for _, v := range req {
			if k, ok := v.(string); ok {
				order = append(order, k)
				seen[k] = true
			}
		}
	}
	var rest []string
	for k := range args {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)
	for i, k := range order {
		if v, ok := args[k]; ok {
			tbl.RawSetInt(i+1, toLua(L, v))
		}
	}
	return tbl
}

func resultString(v lua.LValue) (string, error) {
	switch x := v.(type) {
	case *lua.LNilType:
		return "", nil
	case lua.LString:
		return string(x), nil
	case lua.LNumber:
		return x.String(), nil
	case lua.LBool:
		return strconv.FormatBool(bool(x)), nil
	case *lua.LTable:
		b, err := json.Marshal(fromLua(x))
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return v.String(), nil
	}
}

// ToolSchemas returns the OpenAI tool schemas for every loaded skill.
func (m *Manager) ToolSchemas() []map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.skills))
	for name := range m.skills {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		s := m.skills[name]
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        s.toolName(),
				"description": s.description,
				"parameters":  s.params,
			},
		})
	}
	return out
}

// List returns loaded skills sorted by name.
func (m *Manager) List() []Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.skills)+len(m.errs))
	for name := range m.skills {
		names = append(names, name)
	}
	for name := range m.errs {
		if _, ok := m.skills[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]Info, 0, len(names))
	for _, name := range names {
		if s := m.skills[name]; s != nil {
			out = append(out, *infoOf(s, m.errs[name]))
			continue
		}
		out = append(out, Info{Name: name, Tool: ToolPrefix + name, Persistent: true, Error: m.errs[name]})
	}
	return out
}

func infoOf(s *skill, errText string) *Info {
	return &Info{
		Name:        s.name,
		Description: s.description,
		Tool:        s.toolName(),
		Path:        s.path,
		Persistent:  s.persistent,
		Error:       errText,
	}
}

// Source returns the original Lua source for :skill show and Save.
func (m *Manager) Source(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.skills[name]
	if s == nil {
		return "", false
	}
	return s.source, true
}

// Save writes an ephemeral skill to the skills directory.
func (m *Manager) Save(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.skills[name]
	if s == nil {
		return "", fmt.Errorf("unknown skill %q", name)
	}
	if s.source == "" {
		return "", fmt.Errorf("skill %q has no source", name)
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(m.dir, name+".lua")
	if err := os.WriteFile(path, []byte(s.source), 0o644); err != nil {
		return "", err
	}
	s.persistent = true
	s.path = path
	if info, err := os.Stat(path); err == nil {
		m.files[name] = fileState{mod: info.ModTime().UnixNano(), size: info.Size()}
	}
	return path, nil
}

// Remove deletes an on-disk skill (and any ephemeral session copy).
func (m *Manager) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.skills[name]
	if s == nil {
		return fmt.Errorf("unknown skill %q", name)
	}
	if s.persistent && s.path != "" {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	delete(m.skills, name)
	delete(m.files, name)
	delete(m.errs, name)
	s.close()
	return nil
}

// Close releases every Lua state.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, s := range m.skills {
		s.close()
		delete(m.skills, name)
	}
}

// ---- value conversion ----

func luaString(v lua.LValue) string {
	if s, ok := v.(lua.LString); ok {
		return string(s)
	}
	return ""
}

// normalizeParams accepts the shapes small models tend to produce and turns
// them into a valid object schema: either a full {type,properties,...} schema,
// a bare properties map, or a single-property shorthand.
func normalizeParams(raw map[string]any) map[string]any {
	if raw == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	out := map[string]any{"type": "object"}
	if t, ok := raw["type"].(string); ok && t != "" {
		out["type"] = t
	}
	if p, ok := raw["properties"].(map[string]any); ok {
		out["properties"] = p
	} else {
		props := map[string]any{}
		for k, v := range raw {
			switch k {
			case "type", "required", "properties", "additionalProperties", "description":
				continue
			}
			props[k] = v
		}
		out["properties"] = props
	}
	switch req := raw["required"].(type) {
	case []any:
		out["required"] = req
	case []string:
		out["required"] = req
	case string:
		out["required"] = []any{req}
	}
	if ap, ok := raw["additionalProperties"]; ok {
		out["additionalProperties"] = ap
	}
	if desc, ok := raw["description"]; ok {
		out["description"] = desc
	}
	return out
}

func luaParams(v lua.LValue) (map[string]any, error) {
	if v == lua.LNil {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	tbl, ok := v.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("parameters must be a table")
	}
	m, ok := fromLua(tbl).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parameters must be an object")
	}
	return m, nil
}

func toLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(x)
	case string:
		return lua.LString(x)
	case float64:
		return lua.LNumber(x)
	case float32:
		return lua.LNumber(x)
	case int:
		return lua.LNumber(x)
	case int64:
		return lua.LNumber(x)
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return lua.LNumber(f)
		}
		return lua.LString(x.String())
	case []any:
		t := L.NewTable()
		for i, e := range x {
			t.RawSetInt(i+1, toLua(L, e))
		}
		return t
	case map[string]any:
		t := L.NewTable()
		for k, e := range x {
			t.RawSetString(k, toLua(L, e))
		}
		return t
	default:
		return lua.LString(fmt.Sprint(x))
	}
}

func fromLua(v lua.LValue) any {
	switch x := v.(type) {
	case *lua.LNilType:
		return nil
	case lua.LString:
		return string(x)
	case lua.LNumber:
		return float64(x)
	case lua.LBool:
		return bool(x)
	case *lua.LTable:
		if x.Len() > 0 {
			arr := make([]any, x.Len())
			for i := 1; i <= x.Len(); i++ {
				arr[i-1] = fromLua(x.RawGetInt(i))
			}
			return arr
		}
		out := map[string]any{}
		x.ForEach(func(k, val lua.LValue) {
			out[k.String()] = fromLua(val)
		})
		return out
	default:
		return v.String()
	}
}

// ---- source generation ----

func buildSource(name, description string, params map[string]any, code string) string {
	return fmt.Sprintf("return {\n  name = %s,\n  description = %s,\n  parameters = %s,\n  run = %s,\n}\n",
		luaQuote(name), luaQuote(description), luaLiteral(params), normalizeFunc(code))
}

func normalizeFunc(code string) string {
	c := strings.TrimSpace(code)
	if strings.HasPrefix(c, "return function") {
		c = strings.TrimSpace(strings.TrimPrefix(c, "return"))
	}
	if strings.HasPrefix(c, "function") {
		return c
	}
	return "function(args)\n" + c + "\nend"
}

func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func luaLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "nil"
	case bool:
		return strconv.FormatBool(x)
	case string:
		return luaQuote(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = luaLiteral(e)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, "["+luaQuote(k)+"] = "+luaLiteral(x[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return luaQuote(fmt.Sprint(x))
	}
}
