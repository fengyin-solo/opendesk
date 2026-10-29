package contacts_test

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/useopendesk/opendesk/internal/helpdesk/contacts"
	"github.com/useopendesk/opendesk/internal/helpdesk/hdtest"
	"github.com/useopendesk/opendesk/internal/ids"
)

type labelled struct {
	ID   string `json:"id"`
	Tags []struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	} `json:"tags"`
	Fields map[string]any `json:"fields"`
}

func (l labelled) names() []string {
	out := []string{}
	for _, t := range l.Tags {
		out = append(out, t.Name)
	}
	slices.Sort(out)
	return out
}

func TestContactAndCompanyLabelsAndFields(t *testing.T) {
	e := hdtest.New(t, contacts.Register)
	ws := e.NewWorkspace()
	a := ws.Agent.As(ws)
	gid := ids.New()
	e.Exec(`INSERT INTO tag_groups (id, workspace_id, name, exclusive) VALUES ($1, $2, 'Tier', true)`, gid, ws.ID)
	e.Exec(`INSERT INTO tags (workspace_id, name, group_id, color) VALUES ($1, 'Gold', $2, 'amber'), ($1, 'Silver', $2, 'gray')`, ws.ID, gid)
	e.Exec(`INSERT INTO tags (workspace_id, name, applies_to) VALUES ($1, 'Ticket only', '{tickets}')`, ws.ID)
	e.Exec(`INSERT INTO custom_fields (workspace_id, entity, key, name, type) VALUES ($1, 'contact', 'segment', 'Segment', 'text'),
		($1, 'company', 'seats', 'Seats', 'number')`, ws.ID)

	var c labelled
	e.Do("POST", "/v1/contacts", a, map[string]any{"email": "x@example.com", "tags": []string{"Gold", "vip"},
		"fields": map[string]any{"segment": "Enterprise"}}).Want(t, 201).JSON(t, &c)
	if !slices.Equal(c.names(), []string{"Gold", "vip"}) || c.Fields["segment"] != "Enterprise" {
		t.Fatalf("created %+v", c)
	}
	e.Do("PATCH", "/v1/contacts/"+c.ID, a, map[string]any{"add_tags": []string{"silver"}}).Want(t, 200).JSON(t, &c)
	if !slices.Equal(c.names(), []string{"Silver", "vip"}) {
		t.Fatalf("exclusive %+v", c.names())
	}
	e.Do("PATCH", "/v1/contacts/"+c.ID, a, map[string]any{"add_tags": []string{"Ticket only"}}).Want(t, 422)
	e.Do("PATCH", "/v1/contacts/"+c.ID, a, map[string]any{"fields": map[string]any{"seats": 3}}).Want(t, 422)
	e.Do("PATCH", "/v1/contacts/"+c.ID, ws.Viewer.As(ws), map[string]any{"fields": map[string]any{"segment": "x"}}).Want(t, 403)
	e.Do("POST", "/v1/contacts", a, map[string]any{"email": "y@example.com"}).Want(t, 201)

	var p page[labelled]
	e.Do("GET", "/v1/contacts?tag=silver", a, nil).Want(t, 200).JSON(t, &p)
	if len(p.Data) != 1 || p.Data[0].ID != c.ID {
		t.Fatalf("tag filter %+v", p)
	}
	e.Do("GET", "/v1/contacts?"+url.QueryEscape("field.segment[contains]")+"=prise", a, nil).Want(t, 200).JSON(t, &p)
	if len(p.Data) != 1 {
		t.Fatalf("field filter %+v", p)
	}
	e.Do("GET", "/v1/contacts?q=enterprise", a, nil).Want(t, 200).JSON(t, &p)
	if len(p.Data) != 1 {
		t.Fatalf("search %+v", p)
	}
	e.Do("GET", "/v1/contacts?field.nope=1", a, nil).Want(t, 422)

	// Numeric field filters: non-finite operands are 422 validation_failed naming
	// the field; finite exponent, negative and decimal operands match stored values.
	e.Exec(`INSERT INTO custom_fields (workspace_id, entity, key, name, type) VALUES ($1, 'contact', 'score', 'Score', 'number')`, ws.ID)
	e.Do("PATCH", "/v1/contacts/"+c.ID, a, map[string]any{"fields": map[string]any{"score": 1000}}).Want(t, 200)
	for _, v := range []string{"NaN", "Inf", "-Inf", "Infinity"} {
		res := e.Do("GET", "/v1/contacts?"+url.QueryEscape("field.score[gt]")+"="+v, a, nil)
		if res.Status != 422 || !strings.Contains(string(res.Body), "validation_failed") || !strings.Contains(string(res.Body), "field.score") {
			t.Fatalf("filter %q: %d %s", v, res.Status, res.Body)
		}
	}
	e.Do("GET", "/v1/contacts?"+url.QueryEscape("field.score[eq]")+"=1e3", a, nil).Want(t, 200).JSON(t, &p)
	if len(p.Data) != 1 || p.Data[0].ID != c.ID {
		t.Fatalf("exponent filter %+v", p.Data)
	}
	e.Do("GET", "/v1/contacts?"+url.QueryEscape("field.score[gte]")+"=-1.5", a, nil).Want(t, 200).JSON(t, &p)
	if len(p.Data) != 1 || p.Data[0].ID != c.ID {
		t.Fatalf("negative/decimal filter %+v", p.Data)
	}
	e.Do("GET", "/v1/contacts?"+url.QueryEscape("field.score[set]")+"=", a, nil).Want(t, 200).JSON(t, &p)
	if len(p.Data) != 1 || p.Data[0].ID != c.ID {
		t.Fatalf("set filter %+v", p.Data)
	}
	e.Do("GET", "/v1/contacts?"+url.QueryEscape("field.score[not_set]")+"=", a, nil).Want(t, 200).JSON(t, &p)
	if len(p.Data) != 1 || p.Data[0].ID == c.ID {
		t.Fatalf("not_set filter %+v", p.Data)
	}

	var co labelled
	e.Do("POST", "/v1/companies", a, map[string]any{"name": "Acme", "tags": []string{"Gold"}, "fields": map[string]any{"seats": 40}}).
		Want(t, 201).JSON(t, &co)
	if !slices.Equal(co.names(), []string{"Gold"}) || co.Fields["seats"] != float64(40) {
		t.Fatalf("company %+v", co)
	}
	id := co.ID
	co = labelled{}
	e.Do("PATCH", "/v1/companies/"+id, a, map[string]any{"fields": map[string]any{"seats": nil}, "remove_tags": []string{"gold"}}).
		Want(t, 200).JSON(t, &co)
	if len(co.Tags) != 0 || len(co.Fields) != 0 {
		t.Fatalf("company cleared %+v", co)
	}
	e.Do("GET", "/v1/companies?"+url.QueryEscape("field.seats[set]")+"=", a, nil).Want(t, 200).JSON(t, &p)
	if len(p.Data) != 0 {
		t.Fatalf("company filter %+v", p)
	}
}
