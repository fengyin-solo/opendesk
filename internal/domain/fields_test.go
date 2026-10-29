package domain

import (
	"errors"
	"strings"
	"testing"

	"github.com/useopendesk/opendesk/internal/httpx"
)

func TestCleanFieldValue(t *testing.T) {
	sel := CustomField{Key: "plan", Name: "Plan", Type: FieldMultiSelect, Options: []FieldOption{{ID: "a"}, {ID: "b"}, {ID: "c"}}}
	v, err := CleanFieldValue(sel, []any{"c", "a", "c"}, nil)
	if err != nil || len(v.([]string)) != 2 || v.([]string)[0] != "a" {
		t.Fatalf("%v %v", v, err)
	}
	for _, c := range []struct {
		f  CustomField
		in any
		ok bool
	}{
		{CustomField{Type: FieldNumber}, 1.5, true},
		{CustomField{Type: FieldNumber}, "1", false},
		{CustomField{Type: FieldDate}, "2026-02-30", false},
		{CustomField{Type: FieldDate}, "2026-02-28", true},
		{CustomField{Type: FieldURL}, "javascript:alert(1)", false},
		{CustomField{Type: FieldURL}, "https://example.com/x", true},
		{CustomField{Type: FieldEmail}, "A@Example.com", true},
		{CustomField{Type: FieldEmail}, "nope", false},
		{CustomField{Type: FieldText}, "tab\tin text", false},
		{CustomField{Type: FieldLongText}, "two\nlines", true},
		{CustomField{Type: FieldCheckbox}, "true", false},
		{CustomField{Type: FieldUser}, "not-a-uuid", false},
	} {
		_, err := CleanFieldValue(c.f, c.in, nil)
		if (err == nil) != c.ok {
			t.Errorf("%s %v: err %v", c.f.Type, c.in, err)
		}
	}
}

func TestMatchField(t *testing.T) {
	num := CustomField{Type: FieldNumber}
	if !MatchField(num, "gte", "3", 3.0) || MatchField(num, "lt", "3", 3.0) || !MatchField(num, "not_set", "", nil) {
		t.Fatal("number")
	}
	multi := CustomField{Type: FieldMultiSelect}
	have := []any{"a", "b"}
	if !MatchField(multi, "all", "a,b", have) || MatchField(multi, "all", "a,c", have) || !MatchField(multi, "none", "c", have) {
		t.Fatal("multi")
	}
	cb := CustomField{Type: FieldCheckbox}
	if !MatchField(cb, "eq", "false", nil) || MatchField(cb, "eq", "true", nil) {
		t.Fatal("checkbox")
	}
	txt := CustomField{Type: FieldText}
	if !MatchField(txt, "contains", "ACME", "acme corp") || !MatchField(txt, "ne", "x", nil) {
		t.Fatal("text")
	}
}

func TestCheckFieldOperandNumber(t *testing.T) {
	f := CustomField{Key: "amount", Name: "Amount", Type: FieldNumber}
	// Every comparison op keeps finite numbers, including negatives, decimals,
	// exponent notation and very large finite values; set/not_set take no value.
	ok := []struct {
		op    string
		value string
		want  string
	}{
		{"eq", "0", "0"},
		{"ne", "-1.5", "-1.5"},
		{"gt", "1e3", "1e3"},
		{"gte", "-2.5E2", "-2.5E2"},
		{"lt", "1e308", "1e308"},
		{"lte", " 100.25 ", "100.25"},
		{"eq", "-0.0001", "-0.0001"},
		{"set", "", ""},
		{"not_set", "", ""},
	}
	for _, c := range ok {
		got, err := CheckFieldOperand(f, c.op, c.value, false)
		if err != nil {
			t.Errorf("%s %q: %v", c.op, c.value, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s %q: normalized to %q, want %q", c.op, c.value, got, c.want)
		}
	}
	// NaN and infinities in every spelling ParseFloat accepts are refused, and the
	// error names the field and carries validation_failed.
	for _, v := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "Infinity", "+Infinity", "-Infinity", "infinity"} {
		_, err := CheckFieldOperand(f, "gt", v, false)
		var he *httpx.Error
		if !errors.As(err, &he) {
			t.Errorf("%q: %v", v, err)
			continue
		}
		if he.Code != "validation_failed" || he.Status != 422 {
			t.Errorf("%q: code %s status %d", v, he.Code, he.Status)
		}
		if !strings.Contains(he.Detail, "field.amount") || !strings.Contains(he.Detail, "finite") {
			t.Errorf("%q: detail %q", v, he.Detail)
		}
	}
	// Non-numbers and empty comparison values keep the plain number error.
	for _, v := range []string{"abc", "1.2.3", ""} {
		if _, err := CheckFieldOperand(f, "eq", v, false); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	if _, err := CheckFieldOperand(f, "contains", "1", false); err == nil {
		t.Error("unsupported op accepted")
	}

	// Finite operands still match stored values, including exponent and negatives.
	for _, c := range []struct {
		op   string
		v    string
		have any
		want bool
	}{
		{"eq", "1e3", 1000.0, true},
		{"ne", "1e3", 999.0, true},
		{"gt", "-1.5", -1.0, true},
		{"gte", "100.25", 100.25, true},
		{"lt", "1e308", 1.0, true},
		{"lte", "0", -0.5, true},
		{"set", "", 4.0, true},
		{"not_set", "", nil, true},
	} {
		if got := MatchField(f, c.op, c.v, c.have); got != c.want {
			t.Errorf("%s %s vs %v: %v, want %v", c.op, c.v, c.have, got, c.want)
		}
	}
}

func TestRequiredOnSolve(t *testing.T) {
	defs := FieldDefs{
		"order": {Key: "order", Name: "Order", Type: FieldText, RequiredOnSolve: true},
		"ok":    {Key: "ok", Name: "OK", Type: FieldCheckbox, RequiredOnSolve: true},
		"old":   {Key: "old", Name: "Old", Type: FieldText, RequiredOnSolve: true, Archived: true},
	}
	err := CheckRequiredOnSolve(defs, map[string]any{"ok": false})
	if err == nil || err.Error() != "Fill in OK, Order before solving this ticket." {
		t.Fatalf("%v", err)
	}
	if err := CheckRequiredOnSolve(defs, map[string]any{"ok": true, "order": "1"}); err != nil {
		t.Fatal(err)
	}
}
