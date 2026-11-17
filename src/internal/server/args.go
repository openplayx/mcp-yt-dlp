package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// args validates a tool's arguments object strictly: unknown fields,
// wrong types and out-of-range values are collected as "field: message"
// (inputs are never echoed back).
type args struct {
	raw  map[string]json.RawMessage
	errs []string
	used map[string]bool
	vals map[string]any
}

func mustRe(s string) *regexp.Regexp { return regexp.MustCompile(s) }

func parseArgs(raw json.RawMessage) (*args, bool) {
	a := &args{raw: map[string]json.RawMessage{}, used: map[string]bool{}}
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" || string(t) == "{}" || string(t) == "false" ||
		string(t) == "0" || string(t) == `""` || string(t) == "[]" {
		return a, true // Python: `params.get("arguments") or {}`
	}
	if t[0] != '{' || json.Unmarshal(t, &a.raw) != nil {
		return nil, false
	}
	return a, true
}

func (a *args) fail(field, msg string) { a.errs = append(a.errs, field+": "+msg) }

func (a *args) get(field string) (json.RawMessage, bool) {
	a.used[field] = true
	v, ok := a.raw[field]
	return v, ok
}

func (a *args) str(field string, required bool, def string, minLen, maxLen int) string {
	v, ok := a.get(field)
	if !ok {
		if required {
			a.fail(field, "Field required")
		}
		return def
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		a.fail(field, "Input should be a valid string")
		return def
	}
	n := utf8.RuneCountInString(s)
	if n < minLen {
		a.fail(field, fmt.Sprintf("String should have at least %d character", minLen))
	} else if maxLen > 0 && n > maxLen {
		a.fail(field, fmt.Sprintf("String should have at most %d characters", maxLen))
	}
	return s
}

func (a *args) pattern(field string, value string, re *regexp.Regexp) {
	if value != "" && !re.MatchString(value) {
		a.fail(field, "String should match pattern '"+re.String()+"'")
	}
}

func (a *args) enum(field string, def string, values []string, nullable bool) string {
	v, ok := a.get(field)
	if !ok {
		return def
	}
	if nullable && string(bytes.TrimSpace(v)) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil || !slices.Contains(values, s) {
		a.fail(field, "Input should be "+quoteList(values))
		return def
	}
	return s
}

func (a *args) integer(field string, def, min, max int) int {
	v, ok := a.get(field)
	if !ok {
		return def
	}
	var f float64
	if json.Unmarshal(v, &f) != nil || f != math.Trunc(f) {
		a.fail(field, "Input should be a valid integer")
		return def
	}
	n := int(f)
	if n < min {
		a.fail(field, fmt.Sprintf("Input should be greater than or equal to %d", min))
	} else if n > max {
		a.fail(field, fmt.Sprintf("Input should be less than or equal to %d", max))
	}
	return n
}

func (a *args) boolean(field string, def bool) bool {
	v, ok := a.get(field)
	if !ok {
		return def
	}
	var b bool
	if json.Unmarshal(v, &b) != nil {
		a.fail(field, "Input should be a valid boolean")
		return def
	}
	return b
}

func (a *args) strList(field string, maxItems, maxItemLen int) []string {
	v, ok := a.get(field)
	if !ok {
		return []string{}
	}
	var list []json.RawMessage
	if json.Unmarshal(v, &list) != nil || list == nil {
		a.fail(field, "Input should be a valid list")
		return []string{}
	}
	if len(list) > maxItems {
		a.fail(field, fmt.Sprintf("List should have at most %d items", maxItems))
		return []string{}
	}
	out := []string{}
	for i, item := range list {
		var s string
		if json.Unmarshal(item, &s) != nil {
			a.fail(fmt.Sprintf("%s.%d", field, i), "Input should be a valid string")
			continue
		}
		out = append(out, s)
	}
	return out
}

// done reports unknown fields and returns the joined error text, if any.
func (a *args) done() string {
	var extra []string
	for k := range a.raw {
		if !a.used[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		a.fail(k, "Extra inputs are not permitted")
	}
	if len(a.errs) == 0 {
		return ""
	}
	if len(a.errs) > 5 {
		a.errs = a.errs[:5]
	}
	msg := strings.Join(a.errs, "; ")
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return msg
}

func quoteList(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = "'" + v + "'"
	}
	if len(q) == 1 {
		return q[0]
	}
	return strings.Join(q[:len(q)-1], ", ") + " or " + q[len(q)-1]
}
