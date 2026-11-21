package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
)

const maxConfigBody = 4 << 20

func readConfigBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxConfigBody+1))
	if err != nil {
		return nil, &APIError{400, err.Error()}
	}
	if len(b) > maxConfigBody {
		return nil, &APIError{413, "配置请求体超过 4 MiB"}
	}
	return b, nil
}

// schemaNode 是 JSON 输入的结构化 schema。
// fields 为 nil 表示该节点是用户自定义 map（headers / add_headers）或标量，
// 键由用户决定、不再校验；elem 非 nil 表示数组元素也需要校验。
type schemaNode struct {
	fields map[string]*schemaNode
	elem   *schemaNode
}

func buildSchema(t reflect.Type) *schemaNode {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		n := &schemaNode{fields: make(map[string]*schemaNode, t.NumField())}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" || name == "-" {
				continue
			}
			n.fields[name] = buildSchema(f.Type)
		}
		return n
	case reflect.Slice, reflect.Array:
		return &schemaNode{elem: buildSchema(t.Elem())}
	default:
		return nil
	}
}

var (
	groupSchema    = buildSchema(reflect.TypeOf(GroupConfig{}))
	upstreamSchema = buildSchema(reflect.TypeOf(UpstreamConfig{}))
)

func joinFieldPath(base, key string) string {
	switch {
	case base == "":
		return key
	case strings.HasPrefix(key, "["):
		return base + key
	default:
		return base + "." + key
	}
}

func pathPrefix(base string) string {
	if base == "" {
		return ""
	}
	return base + ": "
}

// redactAPIError 把错误文案里的 URL 统一脱敏后再交给 HTTP 响应。
// typed decode 的报错会原样回显字段值（例如把 URL 填进 duration），
// 这类错误不经过 errString，必须在出口处再兜一层。
func redactAPIError(err error) error {
	if err == nil {
		return nil
	}
	var api *APIError
	if errors.As(err, &api) {
		return &APIError{Status: api.Status, Message: redactURLText(api.Message)}
	}
	return &APIError{400, redactURLText(err.Error())}
}

// validateJSONValue 递归校验已知对象：键必须与 schema 逐字节一致，重复键拒绝。
// headers / add_headers 这类用户自定义 map 的节点为 nil，整段放行。
func validateJSONValue(node *schemaNode, raw json.RawMessage, path string) error {
	if node == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	switch trimmed[0] {
	case '{':
		fields, err := decodeObjectStrict(trimmed)
		if err != nil {
			return &APIError{400, pathPrefix(path) + err.Error()}
		}
		for k, v := range fields {
			child, ok := node.fields[k]
			if !ok {
				return &APIError{400, joinFieldPath(path, k) + ": 未知字段（字段名须与文档完全一致，区分大小写）"}
			}
			if err := validateJSONValue(child, v, joinFieldPath(path, k)); err != nil {
				return err
			}
		}
	case '[':
		if node.elem == nil {
			return nil
		}
		var elems []json.RawMessage
		if err := json.Unmarshal(trimmed, &elems); err != nil {
			return &APIError{400, pathPrefix(path) + "JSON 解析失败: " + err.Error()}
		}
		for i, e := range elems {
			if err := validateJSONValue(node.elem, e, joinFieldPath(path, fmt.Sprintf("[%d]", i))); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeObjectStrict 读取一个 JSON 对象，对键做字节级去重（顶层与嵌套对象共用）。
//
// encoding/json 的 map 解码会静默保留“最后一个重复键”，所以重复键必须
// 在这里就拒绝，否则校验与生效值仍然可能来自不同的键。
func decodeObjectStrict(b []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, &APIError{400, "请求体必须为 JSON 对象"}
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, &APIError{400, "请求体必须为 JSON 对象"}
	}
	fields := make(map[string]json.RawMessage, 8)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, &APIError{400, "JSON 解析失败: " + err.Error()}
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, &APIError{400, "请求体必须为 JSON 对象"}
		}
		if _, dup := fields[key]; dup {
			return nil, &APIError{400, fmt.Sprintf("重复字段: %q", redactURLText(key))}
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, &APIError{400, "JSON 解析失败: " + err.Error()}
		}
		fields[key] = raw
	}
	if _, err := dec.Token(); err != nil { // 闭合 '}'
		return nil, &APIError{400, "JSON 解析失败: " + err.Error()}
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &APIError{400, "请求体只能包含一个 JSON 对象"}
	}
	return fields, nil
}

// decodeFields 先按 schema 逐字节校验全部已知对象（含嵌套），再交给 typed decoder。
//
// 只靠 encoding/json 不够：它做大小写与 Unicode 简单折叠匹配（"Name"、
// "upstreamſ" 都能命中 name/upstreams），而 fields、mergeFields 用的是原始
// 键名。两者不一致会让“校验看到的字段”和“实际生效的字段”分叉。
// 因此这里递归拒绝任何不等于 schema 键名的键（含大小写变体与折叠等价字符）
// 以及同一对象内的重复键；headers / add_headers 是用户自定义 map，整段放行。
func decodeFields(r *http.Request, root *schemaNode, dst any) (map[string]json.RawMessage, error) {
	b, err := readConfigBody(r)
	if err != nil {
		return nil, redactAPIError(err)
	}
	fields, err := decodeObjectStrict(b)
	if err != nil {
		return nil, redactAPIError(err)
	}
	for k, v := range fields {
		child, ok := root.fields[k]
		if !ok {
			return nil, redactAPIError(&APIError{400, fmt.Sprintf("未知字段: %q（字段名须与文档完全一致，区分大小写）", k)})
		}
		if err := validateJSONValue(child, v, k); err != nil {
			return nil, redactAPIError(err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return nil, redactAPIError(&APIError{400, "JSON 解析失败: " + err.Error()})
	}
	return fields, nil
}

func mergeFields(old any, fields map[string]json.RawMessage, dst any) error {
	b, err := json.Marshal(old)
	if err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	for k, v := range fields {
		all[k] = v
	}
	b, err = json.Marshal(all)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func (a *AdminServer) mutationResult(w http.ResponseWriter, method string, err error) {
	if err != nil {
		writeJSON(w, apiErrorStatus(err), map[string]any{"error": redactURLText(err.Error())})
		return
	}
	code := http.StatusOK
	if method == http.MethodPost {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"ok": true})
}

// POST creates; PUT patches supplied fields by immutable name; DELETE uses {"name":"..."}.
// POST may include upstreams for a ready-to-use group; an empty group is allowed for staged CRUD.
func (a *AdminServer) handleGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		writeJSON(w, 405, map[string]any{"error": "只支持 POST/PUT/DELETE"})
		return
	}
	var input GroupConfig
	fields, err := decodeFields(r, groupSchema, &input)
	if err != nil {
		a.mutationResult(w, r.Method, err)
		return
	}
	if input.Name == "" {
		a.mutationResult(w, r.Method, &APIError{400, "name 必填"})
		return
	}
	if r.Method == http.MethodDelete && len(fields) != 1 {
		a.mutationResult(w, r.Method, &APIError{400, "DELETE 只接受 name"})
		return
	}
	if value, ok := fields["listen"]; ok && r.Method != http.MethodDelete {
		var addr string
		if string(value) == "null" || json.Unmarshal(value, &addr) != nil || addr == "" {
			a.mutationResult(w, r.Method, &APIError{400, "listen 不能为空"})
			return
		}
	}
	if value, ok := fields["ua_rules"]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		a.mutationResult(w, r.Method, &APIError{400, "ua_rules 必须为数组（清空请用 []）"})
		return
	}
	if r.Method == http.MethodPut {
		if _, ok := fields["upstreams"]; ok {
			a.mutationResult(w, r.Method, &APIError{400, "请用 upstreams API 修改线路"})
			return
		}
	}
	err = a.manager.Mutate(func(cfg *Config) error {
		g := cfg.Group(input.Name)
		switch r.Method {
		case http.MethodPost:
			if g != nil {
				return &APIError{409, "线路组已存在: " + input.Name}
			}
			if input.Listen == "" {
				return &APIError{400, "listen 必填"}
			}
			cfg.Groups = append(cfg.Groups, input)
		case http.MethodPut:
			if g == nil {
				return &APIError{404, "未知线路组: " + input.Name}
			}
			var updated GroupConfig
			if err := mergeFields(*g, fields, &updated); err != nil {
				return err
			}
			if updated.Name != g.Name {
				return &APIError{400, "name 不可变（重命名请删除后重建）"}
			}
			*g = updated
		case http.MethodDelete:
			if g == nil {
				return &APIError{404, "未知线路组: " + input.Name}
			}
			for i := range cfg.Groups {
				if cfg.Groups[i].Name == input.Name {
					cfg.Groups = append(cfg.Groups[:i], cfg.Groups[i+1:]...)
					break
				}
			}
			for i, name := range cfg.UI.ServiceOrder {
				if name == input.Name {
					cfg.UI.ServiceOrder = append(cfg.UI.ServiceOrder[:i], cfg.UI.ServiceOrder[i+1:]...)
					break
				}
			}
		}
		return nil
	})
	a.mutationResult(w, r.Method, err)
}

func (a *AdminServer) handleGroupUpstreams(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/__linkgate/v1/groups/"), "/")
	if len(parts) == 3 && parts[0] != "" && parts[1] == "upstreams" && parts[2] == "order" {
		a.handleUpstreamOrder(w, r, parts[0])
		return
	}
	if len(parts) != 2 || parts[0] == "" || parts[1] != "upstreams" {
		writeJSON(w, 404, map[string]any{"error": "未知 API 路径"})
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodDelete {
		writeJSON(w, 405, map[string]any{"error": "只支持 POST/PUT/DELETE"})
		return
	}
	var input UpstreamConfig
	fields, err := decodeFields(r, upstreamSchema, &input)
	if err != nil {
		a.mutationResult(w, r.Method, err)
		return
	}
	if input.Name == "" {
		a.mutationResult(w, r.Method, &APIError{400, "name 必填"})
		return
	}
	if r.Method == http.MethodDelete && len(fields) != 1 {
		a.mutationResult(w, r.Method, &APIError{400, "DELETE 只接受 name"})
		return
	}
	err = a.manager.Mutate(func(cfg *Config) error {
		g := cfg.Group(parts[0])
		if g == nil {
			return &APIError{404, "未知线路组: " + parts[0]}
		}
		u := findUpstream(g, input.Name)
		switch r.Method {
		case http.MethodPost:
			if u != nil {
				return &APIError{409, "线路已存在: " + input.Name}
			}
			if input.URL == "" {
				return &APIError{400, "url 必填"}
			}
			g.Upstreams = append(g.Upstreams, input)
		case http.MethodPut:
			if u == nil {
				return &APIError{404, "未知线路: " + input.Name}
			}
			var updated UpstreamConfig
			if err := mergeFields(*u, fields, &updated); err != nil {
				return fmt.Errorf("更新线路: %w", err)
			}
			if updated.Name != u.Name {
				return &APIError{400, "name 不可变（重命名请删除后重建）"}
			}
			*u = updated
		case http.MethodDelete:
			if u == nil {
				return &APIError{404, "未知线路: " + input.Name}
			}
			for i := range g.Upstreams {
				if g.Upstreams[i].Name == input.Name {
					g.Upstreams = append(g.Upstreams[:i], g.Upstreams[i+1:]...)
					break
				}
			}
		}
		return nil
	})
	a.mutationResult(w, r.Method, err)
}
