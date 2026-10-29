package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/useopendesk/opendesk/internal/db"
	"github.com/useopendesk/opendesk/internal/httpx"
)

// Custom fields (contracts/api.md "Labels and custom fields").

const (
	FieldEntityTicket  = "ticket"
	FieldEntityContact = "contact"
	FieldEntityCompany = "company"

	FieldText        = "text"
	FieldLongText    = "long_text"
	FieldNumber      = "number"
	FieldSelect      = "select"
	FieldMultiSelect = "multi_select"
	FieldDate        = "date"
	FieldCheckbox    = "checkbox"
	FieldURL         = "url"
	FieldEmail       = "email"
	FieldUser        = "user"

	MaxFieldsPerEntity = 100
	MaxFieldOptions    = 200
)

var (
	FieldEntities = []string{FieldEntityTicket, FieldEntityContact, FieldEntityCompany}
	FieldTypes    = []string{FieldText, FieldLongText, FieldNumber, FieldSelect, FieldMultiSelect, FieldDate, FieldCheckbox,
		FieldURL, FieldEmail, FieldUser}
	FieldOps = []string{"eq", "ne", "contains", "not_contains", "gt", "gte", "lt", "lte", "any", "all", "none", "set", "not_set"}
)

type FieldOption struct {
	ID    string `json:"id" maxLength:"40"`
	Label string `json:"label" maxLength:"60"`
	Color string `json:"color" enum:"gray,red,orange,amber,yellow,lime,green,teal,cyan,blue,indigo,violet,purple,pink,rose"`
}

type CustomField struct {
	ID              uuid.UUID     `json:"id"`
	Entity          string        `json:"entity" enum:"ticket,contact,company"`
	Key             string        `json:"key"`
	Name            string        `json:"name"`
	Type            string        `json:"type" enum:"text,long_text,number,select,multi_select,date,checkbox,url,email,user"`
	Options         []FieldOption `json:"options"`
	RequiredOnSolve bool          `json:"required_on_solve"`
	Description     string        `json:"description"`
	Position        int           `json:"position"`
	Archived        bool          `json:"archived"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

const CustomFieldSelect = `SELECT id, entity, key, name, type, options, required_on_solve, description, position, archived,
	created_at, updated_at FROM custom_fields`

func ScanCustomField(r pgx.Row) (CustomField, error) {
	var f CustomField
	var opts []byte
	err := r.Scan(&f.ID, &f.Entity, &f.Key, &f.Name, &f.Type, &opts, &f.RequiredOnSolve, &f.Description, &f.Position,
		&f.Archived, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return f, err
	}
	_ = json.Unmarshal(opts, &f.Options)
	if f.Options == nil {
		f.Options = []FieldOption{}
	}
	return f, nil
}

// FieldDefs are an entity's field definitions by key.
type FieldDefs map[string]CustomField

// LoadFieldDefs loads the workspace's field definitions for an entity, archived included.
func LoadFieldDefs(ctx context.Context, q db.Querier, wid uuid.UUID, entity string) (FieldDefs, error) {
	rows, err := q.Query(ctx, CustomFieldSelect+` WHERE workspace_id = $1 AND entity = $2 ORDER BY position, key`, wid, entity)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CustomField, error) { return ScanCustomField(r) })
	if err != nil {
		return nil, err
	}
	defs := FieldDefs{}
	for _, f := range list {
		defs[f.Key] = f
	}
	return defs, nil
}

// Ordered returns the definitions by position.
func (d FieldDefs) Ordered() []CustomField {
	out := make([]CustomField, 0, len(d))
	for _, f := range d {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b CustomField) int {
		if a.Position != b.Position {
			return a.Position - b.Position
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out
}

func (f CustomField) hasOption(id string) bool {
	return slices.ContainsFunc(f.Options, func(o FieldOption) bool { return o.ID == id })
}

func (f CustomField) optionIndex(id string) int {
	return slices.IndexFunc(f.Options, func(o FieldOption) bool { return o.ID == id })
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func fieldErr(f CustomField, msg string) error {
	return &httpx.Error{Status: 422, Title: "Unprocessable Entity", Code: "validation_failed", Detail: f.Name + ": " + msg,
		Errors: []*huma.ErrorDetail{{Location: "body.fields." + f.Key, Message: msg}}}
}

func cleanFieldText(f CustomField, s string, max int, multiline bool) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		return "", fieldErr(f, fmt.Sprintf("must be %d characters or less", max))
	}
	for _, r := range s {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t' || r == '\r')) {
			return "", fieldErr(f, "contains characters that are not allowed")
		}
	}
	return s, nil
}

// MemberCheck confirms a user id is a member of the workspace (for user fields).
type MemberCheck func(uuid.UUID) error

// CleanFieldValue validates one value for f and returns its stored form; nil means clear.
func CleanFieldValue(f CustomField, v any, member MemberCheck) (any, error) {
	if v == nil {
		return nil, nil
	}
	str := func() (string, error) {
		s, ok := v.(string)
		if !ok {
			return "", fieldErr(f, "must be a string")
		}
		return s, nil
	}
	switch f.Type {
	case FieldText, FieldLongText:
		s, err := str()
		if err != nil {
			return nil, err
		}
		max := 1000
		if f.Type == FieldLongText {
			max = 10000
		}
		s, err = cleanFieldText(f, s, max, f.Type == FieldLongText)
		if err != nil || s == "" {
			return nil, err
		}
		return s, nil
	case FieldNumber:
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case int:
			n = float64(x)
		case int64:
			n = float64(x)
		case json.Number:
			p, err := x.Float64()
			if err != nil {
				return nil, fieldErr(f, "must be a number")
			}
			n = p
		default:
			return nil, fieldErr(f, "must be a number")
		}
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) > 1e15 {
			return nil, fieldErr(f, "must be a finite number")
		}
		return n, nil
	case FieldCheckbox:
		b, ok := v.(bool)
		if !ok {
			return nil, fieldErr(f, "must be true or false")
		}
		return b, nil
	case FieldDate:
		s, err := str()
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		if _, err := time.Parse("2006-01-02", s); err != nil || !dateRe.MatchString(s) {
			return nil, fieldErr(f, "must be a date like 2026-10-11")
		}
		return s, nil
	case FieldURL:
		s, err := str()
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(s) > 2000 || strings.ContainsAny(s, "<>\"' ") {
			return nil, fieldErr(f, "must be an http or https URL")
		}
		return s, nil
	case FieldEmail:
		s, err := str()
		if err != nil {
			return nil, err
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			return nil, nil
		}
		a, err := mail.ParseAddress(s)
		if err != nil || a.Address != s || len(s) > 254 {
			return nil, fieldErr(f, "must be an email address")
		}
		return s, nil
	case FieldSelect:
		s, err := str()
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		if !f.hasOption(s) {
			return nil, fieldErr(f, fmt.Sprintf("%q is not one of its options", truncateRunes(s, 40)))
		}
		return s, nil
	case FieldMultiSelect:
		var list []string
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				s, ok := e.(string)
				if !ok {
					return nil, fieldErr(f, "must be a list of option ids")
				}
				list = append(list, s)
			}
		case []string:
			list = x
		default:
			return nil, fieldErr(f, "must be a list of option ids")
		}
		out := []string{}
		for _, s := range list {
			s = strings.TrimSpace(s)
			if !f.hasOption(s) {
				return nil, fieldErr(f, fmt.Sprintf("%q is not one of its options", truncateRunes(s, 40)))
			}
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		// Stored in option order, so equal sets compare equal.
		slices.SortFunc(out, func(a, b string) int { return f.optionIndex(a) - f.optionIndex(b) })
		return out, nil
	case FieldUser:
		s, err := str()
		if err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, nil
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, fieldErr(f, "must be a user id")
		}
		if member != nil {
			if err := member(id); err != nil {
				return nil, fieldErr(f, "must be a member of this workspace")
			}
		}
		return id.String(), nil
	}
	return nil, fieldErr(f, "has an unknown type")
}

// FieldChange is one changed key, for field_changed activities.
type FieldChange struct {
	Key, Name string
	From, To  any
}

// MergeFields validates patch against defs and merges it into cur (null or empty clears).
// It returns the new values and the changes in key order. Unknown keys and changes to
// archived fields are refused.
func MergeFields(defs FieldDefs, cur, patch map[string]any, member MemberCheck) (map[string]any, []FieldChange, error) {
	next := map[string]any{}
	for k, v := range cur {
		next[k] = v
	}
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var changes []FieldChange
	for _, k := range keys {
		f, ok := defs[k]
		if !ok {
			return nil, nil, &httpx.Error{Status: 422, Title: "Unprocessable Entity", Code: "validation_failed",
				Detail: fmt.Sprintf("There is no custom field %q.", truncateRunes(k, 40)),
				Errors: []*huma.ErrorDetail{{Location: "body.fields." + truncateRunes(k, 40), Message: "unknown field"}}}
		}
		v, err := CleanFieldValue(f, patch[k], member)
		if err != nil {
			return nil, nil, err
		}
		old, had := cur[k]
		if SameFieldValue(old, v) && (had || v == nil) {
			continue
		}
		if f.Archived {
			return nil, nil, fieldErr(f, "is archived and cannot be changed")
		}
		if v == nil {
			delete(next, k)
		} else {
			next[k] = v
		}
		changes = append(changes, FieldChange{Key: k, Name: f.Name, From: old, To: v})
	}
	return next, changes, nil
}

// SameFieldValue compares two stored values.
func SameFieldValue(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// FieldEmpty reports whether a value does not satisfy "required": absent, or an unchecked checkbox.
func FieldEmpty(f CustomField, v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case []string:
		return len(x) == 0
	case bool:
		return f.Type == FieldCheckbox && !x
	}
	return false
}

// CheckRequiredOnSolve answers 422 field_required when a required ticket field is empty.
func CheckRequiredOnSolve(defs FieldDefs, values map[string]any) error {
	var missing []CustomField
	for _, f := range defs.Ordered() {
		if f.RequiredOnSolve && !f.Archived && FieldEmpty(f, values[f.Key]) {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	names := make([]string, 0, len(missing))
	e := &httpx.Error{Status: 422, Title: "Unprocessable Entity", Code: "field_required"}
	for _, f := range missing {
		names = append(names, f.Name)
		e.Errors = append(e.Errors, &huma.ErrorDetail{Location: "body.fields." + f.Key, Message: f.Name + " is required to solve"})
	}
	e.Detail = "Fill in " + strings.Join(names, ", ") + " before solving this ticket."
	return e
}

// ParseFieldString turns the string form (automation set_field values) into a value for f.
func ParseFieldString(f CustomField, s string, member MemberCheck) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var v any = s
	switch f.Type {
	case FieldNumber:
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fieldErr(f, "must be a number")
		}
		v = n
	case FieldCheckbox:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fieldErr(f, `must be "true" or "false"`)
		}
		v = b
	case FieldMultiSelect:
		parts := []any{}
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		v = parts
	}
	return CleanFieldValue(f, v, member)
}

// OpsFor lists the filter and condition ops a field type supports.
func OpsFor(typ string) []string {
	switch typ {
	case FieldText, FieldLongText, FieldURL, FieldEmail:
		return []string{"eq", "ne", "contains", "not_contains", "set", "not_set"}
	case FieldNumber, FieldDate:
		return []string{"eq", "ne", "gt", "gte", "lt", "lte", "set", "not_set"}
	case FieldCheckbox:
		return []string{"eq", "ne", "set", "not_set"}
	case FieldSelect, FieldUser:
		return []string{"eq", "ne", "any", "none", "set", "not_set"}
	case FieldMultiSelect:
		return []string{"any", "all", "none", "eq", "ne", "set", "not_set"}
	}
	return nil
}

// DefaultOp is the op of `field.<key>=v` without an explicit op.
func DefaultOp(typ string) string {
	switch typ {
	case FieldSelect, FieldMultiSelect, FieldUser:
		return "any"
	}
	return "eq"
}

// NormalizeOp maps condition spellings onto the field ops.
func NormalizeOp(op string) string {
	switch op {
	case "equals":
		return "eq"
	case "not_equals":
		return "ne"
	}
	return op
}

// SplitList splits a comma separated op value.
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// CheckFieldOperand validates op and value for f (filters and conditions). allowMe accepts
// "me" for user fields. It returns the normalized value.
func CheckFieldOperand(f CustomField, op, value string, allowMe bool) (string, error) {
	if !slices.Contains(OpsFor(f.Type), op) {
		return "", httpx.Validation(fmt.Sprintf("field.%s supports the ops %s", f.Key, strings.Join(OpsFor(f.Type), ", ")))
	}
	value = strings.TrimSpace(value)
	if op == "set" || op == "not_set" {
		return "", nil
	}
	if utf8.RuneCountInString(value) > 500 {
		return "", httpx.Validation("field filter values must be 500 characters or less")
	}
	bad := func(msg string) error { return httpx.Validation(fmt.Sprintf("field.%s: %s", f.Key, msg)) }
	switch f.Type {
	case FieldNumber:
		// Number operands are finite, the same invariant stored field values keep.
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return "", bad("value must be a number")
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return "", bad("value must be a finite number")
		}
	case FieldDate:
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return "", bad("value must be a date like 2026-10-11")
		}
	case FieldCheckbox:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return "", bad(`value must be "true" or "false"`)
		}
		value = strconv.FormatBool(b)
	case FieldSelect, FieldMultiSelect:
		list := SplitList(value)
		if len(list) == 0 {
			return "", bad("value needs an option id")
		}
		for _, o := range list {
			if !f.hasOption(o) {
				return "", bad(fmt.Sprintf("%q is not one of its options", truncateRunes(o, 40)))
			}
		}
		if (op == "eq" || op == "ne") && len(list) > 1 {
			return "", bad("eq and ne take one option; use any, all or none for several")
		}
		value = strings.Join(list, ",")
	case FieldUser:
		list := SplitList(value)
		if len(list) == 0 {
			return "", bad("value needs a user id")
		}
		for _, u := range list {
			if u == "me" && allowMe {
				continue
			}
			if _, err := uuid.Parse(u); err != nil {
				return "", bad("value must be user ids")
			}
		}
		value = strings.Join(list, ",")
	}
	return value, nil
}

// MatchField evaluates op and value (as CheckFieldOperand returned) against a stored value.
func MatchField(f CustomField, op, value string, have any) bool {
	empty := have == nil || FieldEmpty(CustomField{Type: FieldText}, have)
	switch op {
	case "set":
		return !empty
	case "not_set":
		return empty
	}
	switch f.Type {
	case FieldText, FieldLongText, FieldURL, FieldEmail:
		h, _ := have.(string)
		h, w := strings.ToLower(h), strings.ToLower(value)
		switch op {
		case "eq":
			return h == w
		case "ne":
			return h != w
		case "contains":
			return strings.Contains(h, w)
		case "not_contains":
			return !strings.Contains(h, w)
		}
	case FieldNumber:
		h, ok := have.(float64)
		w, _ := strconv.ParseFloat(value, 64)
		if !ok {
			return op == "ne"
		}
		return compareOrdered(op, h, w)
	case FieldDate:
		h, ok := have.(string)
		if !ok {
			return op == "ne"
		}
		return compareOrdered(op, h, value)
	case FieldCheckbox:
		h, _ := have.(bool)
		w := value == "true"
		if op == "eq" {
			return h == w
		}
		return h != w
	case FieldSelect, FieldUser:
		h, _ := have.(string)
		list := SplitList(value)
		switch op {
		case "eq", "any":
			return slices.Contains(list, h)
		case "ne", "none":
			return !slices.Contains(list, h)
		}
	case FieldMultiSelect:
		var hs []string
		switch x := have.(type) {
		case []any:
			for _, e := range x {
				if s, ok := e.(string); ok {
					hs = append(hs, s)
				}
			}
		case []string:
			hs = x
		}
		list := SplitList(value)
		hasAny := slices.ContainsFunc(list, func(o string) bool { return slices.Contains(hs, o) })
		switch op {
		case "any", "eq":
			return hasAny
		case "none", "ne":
			return !hasAny
		case "all":
			return !slices.ContainsFunc(list, func(o string) bool { return !slices.Contains(hs, o) })
		}
	}
	return false
}

func compareOrdered[T float64 | string](op string, h, w T) bool {
	switch op {
	case "eq":
		return h == w
	case "ne":
		return h != w
	case "gt":
		return h > w
	case "gte":
		return h >= w
	case "lt":
		return h < w
	case "lte":
		return h <= w
	}
	return false
}

// MemberChecker returns a MemberCheck against the workspace's members.
func MemberChecker(ctx context.Context, q db.Querier, wid uuid.UUID) MemberCheck {
	return func(uid uuid.UUID) error {
		var ok bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM members WHERE workspace_id = $1 AND user_id = $2)`, wid, uid).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("not a member")
		}
		return nil
	}
}
