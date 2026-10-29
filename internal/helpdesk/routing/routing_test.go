package routing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/useopendesk/opendesk/internal/auth"
	"github.com/useopendesk/opendesk/internal/crypto"
	"github.com/useopendesk/opendesk/internal/deps"
	"github.com/useopendesk/opendesk/internal/domain"
	"github.com/useopendesk/opendesk/internal/helpdesk/hdtest"
	"github.com/useopendesk/opendesk/internal/httpx"
	"github.com/useopendesk/opendesk/internal/ids"
)

func TestRuleOutcome(t *testing.T) {
	mbox := ids.New()
	f := &facts{in: Input{Channel: "email", FromEmail: "Ann@Big.Example", Subject: "Invoice #4 overdue", Body: "please help",
		ReceivedAt: "sales@acme.test", MailboxID: &mbox, PageURL: "https://acme.test/pricing", Topic: "billing"}}
	c := func(field, op, value string) RoutingCondition {
		return RoutingCondition{Field: field, Op: op, Value: value}
	}
	decide := RoutingCondition{Field: FieldAIDecide, Op: "gt", Value: "0.5", Question: "Is this about money?"}
	cases := []struct {
		name string
		rule RoutingRule
		want outcome
	}{
		{"all hold", RoutingRule{Match: MatchAll, Conditions: []RoutingCondition{c("subject", "contains", "INVOICE"), c("from_domain", "equals", "big.example")}}, yes},
		{"one fails", RoutingRule{Match: MatchAll, Conditions: []RoutingCondition{c("subject", "contains", "invoice"), c("channel", "equals", "chat")}}, no},
		{"any holds", RoutingRule{Match: MatchAny, Conditions: []RoutingCondition{c("channel", "equals", "chat"), c("received_at", "equals", "SALES@acme.test")}}, yes},
		{"any none", RoutingRule{Match: MatchAny, Conditions: []RoutingCondition{c("channel", "equals", "chat"), c("topic", "equals", "sales")}}, no},
		{"starts_with", RoutingRule{Match: MatchAll, Conditions: []RoutingCondition{c("page_url", "starts_with", "https://acme.test/")}}, yes},
		{"not_contains", RoutingRule{Match: MatchAll, Conditions: []RoutingCondition{c("body", "not_contains", "refund")}}, yes},
		{"mailbox id", RoutingRule{Match: MatchAll, Conditions: []RoutingCondition{c("mailbox_id", "equals", mbox.String())}}, yes},
		{"no widget", RoutingRule{Match: MatchAll, Conditions: []RoutingCondition{c("widget_id", "equals", "")}}, yes},
		// ai_decide is not answered at creation.
		{"all with decide", RoutingRule{Match: MatchAll, Conditions: []RoutingCondition{c("subject", "contains", "invoice"), decide}}, unknown},
		{"all, decide and a failure", RoutingRule{Match: MatchAll, Conditions: []RoutingCondition{decide, c("channel", "equals", "chat")}}, no},
		{"any, decide and a match", RoutingRule{Match: MatchAny, Conditions: []RoutingCondition{decide, c("channel", "equals", "email")}}, yes},
		{"any, only decide", RoutingRule{Match: MatchAny, Conditions: []RoutingCondition{decide, c("channel", "equals", "chat")}}, unknown},
		{"empty", RoutingRule{Match: MatchAll}, no},
	}
	for _, tc := range cases {
		got, err := f.rule(context.Background(), nil, tc.rule)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func register(d *deps.Deps, api huma.API, _ chi.Router) { Register(d, api) }

type world struct {
	e       *hdtest.Env
	ws      hdtest.WS
	brand   uuid.UUID
	support uuid.UUID
	billing uuid.UUID
	mailbox uuid.UUID
	widget  uuid.UUID
}

func newWorld(t *testing.T) *world {
	e := hdtest.New(t, register)
	w := &world{e: e, ws: e.NewWorkspace(), support: ids.New(), billing: ids.New(), mailbox: ids.New(), widget: ids.New()}
	w.brand = w.ws.BrandID
	e.Exec(`INSERT INTO teams (id, workspace_id, name) VALUES ($1, $2, 'Support'), ($3, $2, 'Billing')`, w.support, w.ws.ID, w.billing)
	e.Exec(`INSERT INTO mailboxes (id, workspace_id, name, email, inbound_kind, forwarding_token, outbound_kind, team_id, brand_id, aliases)
		VALUES ($1, $2, 'Help', $3, 'forwarding', $4, 'system', $5, $6, '{sales@acme.test}')`,
		w.mailbox, w.ws.ID, "help-"+crypto.RandomString(6)+"@acme.test", "tok-"+crypto.RandomString(8), w.support, w.brand)
	e.Exec(`INSERT INTO widgets (id, workspace_id, brand_id, name, key, team_id, settings) VALUES ($1, $2, $3, 'Site', $4, $5, $6)`,
		w.widget, w.ws.ID, w.brand, "wk_"+crypto.RandomString(16), w.support,
		map[string]any{"topics": []map[string]any{{"id": "money", "label": "Billing", "team_id": w.billing.String()}, {"id": "other", "label": "Other", "team_id": nil}}})
	return w
}

func (w *world) admin() hdtest.Auth { return w.ws.Admin.As(w.ws) }

func TestRoutingRulesAPI(t *testing.T) {
	w := newWorld(t)
	e, ad := w.e, w.admin()
	var r RoutingRule
	e.Do("POST", "/v1/routing-rules", ad, map[string]any{"name": "  Invoices  ", "team_id": w.billing,
		"conditions": []map[string]any{{"field": "subject", "op": "contains", "value": "invoice"}, {"field": "from_domain", "op": "equals", "value": "@Big.Example"}}}).
		Want(t, 201).JSON(t, &r)
	if r.Name != "Invoices" || !r.Enabled || r.Match != MatchAll || r.TeamID != w.billing || r.Position != 0 || r.Conditions[1].Value != "big.example" {
		t.Fatalf("rule %+v", r)
	}
	var r2 RoutingRule
	e.Do("POST", "/v1/routing-rules", ad, map[string]any{"name": "Chat pricing", "team_id": w.support, "match": "any",
		"conditions": []map[string]any{{"field": "widget_id", "op": "equals", "value": w.widget}, {"field": "ai_decide", "op": "gt", "value": "0.7", "question": "Is it urgent?"}}}).
		Want(t, 201).JSON(t, &r2)
	if r2.Position != 1 || r2.Conditions[1].Question != "Is it urgent?" {
		t.Fatalf("rule 2 %+v", r2)
	}

	other := e.NewWorkspace()
	foreignTeam := ids.New()
	e.Exec(`INSERT INTO teams (id, workspace_id, name) VALUES ($1, $2, 'Theirs')`, foreignTeam, other.ID)
	cond := func(field, op, value string) []map[string]any {
		return []map[string]any{{"field": field, "op": op, "value": value}}
	}
	bad := []map[string]any{
		{"name": "x", "team_id": w.billing, "conditions": []map[string]any{}},
		{"name": "", "team_id": w.billing, "conditions": cond("subject", "contains", "a")},
		{"name": strings.Repeat("n", 81), "team_id": w.billing, "conditions": cond("subject", "contains", "a")},
		{"name": "x <b>", "team_id": w.billing, "conditions": cond("subject", "contains", "a")},
		{"name": "x", "team_id": foreignTeam, "conditions": cond("subject", "contains", "a")},
		{"name": "x", "conditions": cond("subject", "contains", "a")},
		{"name": "x", "team_id": w.billing, "conditions": cond("priority", "equals", "high")},
		{"name": "x", "team_id": w.billing, "conditions": cond("subject", "gt", "a")},
		{"name": "x", "team_id": w.billing, "conditions": cond("subject", "contains", "")},
		{"name": "x", "team_id": w.billing, "conditions": cond("channel", "equals", "fax")},
		{"name": "x", "team_id": w.billing, "conditions": cond("mailbox_id", "equals", ids.New().String())},
		{"name": "x", "team_id": w.billing, "conditions": cond("brand_id", "contains", w.brand.String())},
		{"name": "x", "team_id": w.billing, "conditions": cond("contact.field.nope", "eq", "a")},
		{"name": "x", "team_id": w.billing, "conditions": []map[string]any{{"field": "ai_decide", "op": "gt", "value": "2", "question": "Urgent?"}}},
	}
	many := make([]map[string]any, 21)
	for i := range many {
		many[i] = map[string]any{"field": "subject", "op": "contains", "value": "a"}
	}
	bad = append(bad, map[string]any{"name": "x", "team_id": w.billing, "conditions": many})
	for _, b := range bad {
		if res := e.Do("POST", "/v1/routing-rules", ad, b); res.Status != 422 {
			t.Errorf("%v: %d %s", b, res.Status, res.Body)
		}
	}
	// A contact field condition takes the ops of its type.
	e.Exec(`INSERT INTO custom_fields (workspace_id, entity, key, name, type) VALUES ($1, 'contact', 'plan', 'Plan', 'text')`, w.ws.ID)
	e.Do("POST", "/v1/routing-rules", ad, map[string]any{"name": "Plan", "team_id": w.billing, "enabled": false,
		"conditions": cond("contact.field.plan", "equals", "Enterprise")}).Want(t, 201)

	// Admins only, and keys with the settings scopes.
	e.Do("GET", "/v1/routing-rules", w.ws.Agent.As(w.ws), nil).Want(t, 403)
	e.Do("POST", "/v1/routing-rules", w.ws.Agent.As(w.ws), map[string]any{"name": "x", "team_id": w.billing, "conditions": cond("subject", "contains", "a")}).Want(t, 403)
	key := e.APIKey(w.ws, auth.ScopeSettingsRead)
	var list struct{ Data []RoutingRule }
	e.Do("GET", "/v1/routing-rules", hdtest.Auth{Bearer: key}, nil).Want(t, 200).JSON(t, &list)
	if len(list.Data) != 3 || list.Data[0].ID != r.ID {
		t.Fatalf("list %+v", list.Data)
	}
	e.Do("GET", "/v1/routing-rules/"+r.ID.String(), other.Admin.As(other), nil).Want(t, 404)

	var patched RoutingRule
	e.Do("PATCH", "/v1/routing-rules/"+r.ID.String(), ad, map[string]any{"enabled": false, "match": "any"}).Want(t, 200).JSON(t, &patched)
	if patched.Enabled || patched.Match != MatchAny || len(patched.Conditions) != 2 {
		t.Fatalf("patched %+v", patched)
	}
	e.Do("PATCH", "/v1/routing-rules/"+r.ID.String(), ad, map[string]any{"conditions": []map[string]any{}}).Want(t, 422)

	// Reorder takes every rule once.
	e.Do("POST", "/v1/routing-rules/reorder", ad, map[string]any{"ids": []string{r2.ID.String(), r.ID.String()}}).Want(t, 422)
	ids3 := []string{r2.ID.String(), list.Data[2].ID.String(), r.ID.String()}
	e.Do("POST", "/v1/routing-rules/reorder", ad, map[string]any{"ids": ids3}).Want(t, 200).JSON(t, &list)
	if list.Data[0].ID != r2.ID || list.Data[0].Position != 0 || list.Data[2].ID != r.ID || list.Data[2].Position != 2 {
		t.Fatalf("reordered %+v", list.Data)
	}

	e.Do("DELETE", "/v1/routing-rules/"+r.ID.String(), ad, nil).Want(t, 204)
	e.Do("DELETE", "/v1/routing-rules/"+r.ID.String(), ad, nil).Want(t, 404)

	types, _ := e.Events(w.ws.ID, 0)
	joined := strings.Join(types, ",")
	for _, typ := range []string{"routing_rule.created", "routing_rule.updated", "routing_rule.deleted"} {
		if !strings.Contains(joined, typ) {
			t.Errorf("event %s missing: %s", typ, joined)
		}
	}
	var audits int
	e.Scalar(`SELECT count(*) FROM audit_logs WHERE workspace_id = $1 AND action LIKE 'routing_rule.%'`, &audits, w.ws.ID)
	if audits < 6 {
		t.Errorf("audit rows %d", audits)
	}

	// The limit.
	e.Exec(`INSERT INTO routing_rules (workspace_id, name, conditions, team_id, position)
		SELECT $1, 'r' || g, '[{"field":"subject","op":"contains","value":"x"}]', $2, g + 10 FROM generate_series(1, 198) g`, w.ws.ID, w.billing)
	res := e.Do("POST", "/v1/routing-rules", ad, map[string]any{"name": "one more", "team_id": w.billing, "conditions": cond("subject", "contains", "a")})
	if res.Status != 422 || !strings.Contains(string(res.Body), "limit_reached") {
		t.Fatalf("limit: %d %s", res.Status, res.Body)
	}
}

func TestResolve(t *testing.T) {
	w := newWorld(t)
	ctx, q := context.Background(), w.e.D.DB.Pool
	in := Input{WorkspaceID: w.ws.ID, Channel: domain.ChannelEmail, BrandID: w.brand, MailboxID: &w.mailbox, ReceivedAt: "sales@acme.test",
		FromEmail: "ann@big.example", Subject: "Hello", Body: "hi"}

	res, err := Resolve(ctx, q, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.TeamID == nil || *res.TeamID != w.support || res.Routed.Via != ViaAddress || res.RuleID() != nil {
		t.Fatalf("address: %+v %+v", res.TeamID, res.Routed)
	}

	// Chat: the topic's team, else the widget's.
	chat := Input{WorkspaceID: w.ws.ID, Channel: domain.ChannelChat, BrandID: w.brand, WidgetID: &w.widget, Topic: "money"}
	if res, _ = Resolve(ctx, q, chat); res.TeamID == nil || *res.TeamID != w.billing || res.Routed.Via != ViaTopic {
		t.Fatalf("topic: %+v", res.Routed)
	}
	chat.Topic = "other"
	if res, _ = Resolve(ctx, q, chat); res.TeamID == nil || *res.TeamID != w.support || res.Routed.Via != ViaWidget {
		t.Fatalf("widget: %+v", res.Routed)
	}
	// API: the caller's team wins over the address's.
	api := in
	api.Channel, api.TeamID = domain.ChannelAPI, &w.billing
	if res, _ = Resolve(ctx, q, api); *res.TeamID != w.billing || res.Routed.Via != ViaAPI {
		t.Fatalf("api: %+v", res.Routed)
	}

	// Rules in position order; disabled rules are skipped; ai_decide is unknown.
	decide := ids.New()
	w.e.Exec(`INSERT INTO routing_rules (id, workspace_id, name, conditions, team_id, position) VALUES ($1, $2, 'Ask', $3, $4, 0)`,
		decide, w.ws.ID, `[{"field":"ai_decide","op":"gt","value":"0.5","question":"Is it billing?"}]`, w.billing)
	w.e.Exec(`INSERT INTO routing_rules (workspace_id, name, conditions, team_id, position, enabled) VALUES ($1, 'Off', $2, $3, 1, false)`,
		w.ws.ID, `[{"field":"channel","op":"equals","value":"email"}]`, w.billing)
	match := ids.New()
	w.e.Exec(`INSERT INTO routing_rules (id, workspace_id, name, conditions, team_id, position) VALUES ($1, $2, 'Big', $3, $4, 2)`,
		match, w.ws.ID, `[{"field":"from_domain","op":"equals","value":"big.example"},{"field":"received_at","op":"equals","value":"sales@acme.test"}]`, w.billing)
	w.e.Exec(`INSERT INTO routing_rules (workspace_id, name, conditions, team_id, position) VALUES ($1, 'Later', $2, $3, 3)`,
		w.ws.ID, `[{"field":"channel","op":"equals","value":"email"}]`, w.support)
	res, err = Resolve(ctx, q, in)
	if err != nil {
		t.Fatal(err)
	}
	if *res.TeamID != w.billing || res.Routed.Via != ViaRule || res.RuleID() == nil || *res.RuleID() != match || res.Routed.RuleName != "Big" {
		t.Fatalf("rule: %+v", res.Routed)
	}
	if len(res.Steps) != 2 || res.Steps[0].Matched != Unknown || res.Steps[1].Matched != true {
		t.Fatalf("steps %+v", res.Steps)
	}
	if err := Count(ctx, q, match); err != nil {
		t.Fatal(err)
	}
	var runs int64
	w.e.Scalar(`SELECT run_count FROM routing_rules WHERE id = $1`, &runs, match)
	if runs != 1 {
		t.Fatalf("run_count %d", runs)
	}

	// Contact fields.
	w.e.Exec(`DELETE FROM routing_rules WHERE workspace_id = $1`, w.ws.ID)
	w.e.Exec(`INSERT INTO custom_fields (workspace_id, entity, key, name, type) VALUES ($1, 'contact', 'plan', 'Plan', 'text')`, w.ws.ID)
	contact := ids.New()
	w.e.Exec(`INSERT INTO contacts (id, workspace_id, email, name, fields) VALUES ($1, $2, 'ann@big.example', 'Ann', '{"plan":"Enterprise"}')`, contact, w.ws.ID)
	w.e.Exec(`INSERT INTO routing_rules (workspace_id, name, conditions, team_id) VALUES ($1, 'VIP', '[{"field":"contact.field.plan","op":"eq","value":"enterprise"}]', $2)`,
		w.ws.ID, w.billing)
	in.ContactID = &contact
	if res, _ = Resolve(ctx, q, in); *res.TeamID != w.billing || res.Routed.Via != ViaRule {
		t.Fatalf("contact field: %+v", res.Routed)
	}

	// The routing test answers where a message would go, without writing anything.
	var out RoutingTestResult
	w.e.Do("POST", "/v1/routing/test", w.admin(), map[string]any{"channel": "email", "to": "Sales@acme.test", "from": "ann@big.example", "subject": "x"}).
		Want(t, 200).JSON(t, &out)
	if out.MailboxID == nil || *out.MailboxID != w.mailbox || out.BrandID != w.brand || out.TeamID == nil || *out.TeamID != w.billing ||
		len(out.Steps) != 1 || out.Steps[0].Matched != true || out.AssignMode != "none" {
		t.Fatalf("test %+v", out)
	}
	w.e.Do("POST", "/v1/routing/test", w.admin(), map[string]any{"channel": "chat", "widget_id": w.widget, "topic": "money"}).Want(t, 200).JSON(t, &out)
	if out.TeamID == nil || *out.TeamID != w.billing || out.Routed.Via != ViaTopic {
		t.Fatalf("chat test %+v", out)
	}
	w.e.Do("POST", "/v1/routing/test", w.admin(), map[string]any{"channel": "chat", "widget_id": ids.New()}).Want(t, 422)
	w.e.Do("POST", "/v1/routing/test", w.ws.Agent.As(w.ws), map[string]any{"channel": "email"}).Want(t, 403)
	w.e.Scalar(`SELECT run_count FROM routing_rules WHERE workspace_id = $1 AND name = 'VIP'`, &runs, w.ws.ID)
	if runs != 0 {
		t.Fatalf("test counted a run: %d", runs)
	}
}

func TestContactNumberFieldConditions(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.e.Exec(`INSERT INTO custom_fields (workspace_id, entity, key, name, type) VALUES ($1, 'contact', 'seats', 'Seats', 'number')`, w.ws.ID)
	nonFinite := []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "Infinity", "+Infinity", "-Infinity"}

	// The constructor refuses every non-finite spelling and names the field.
	for _, v := range nonFinite {
		_, err := ValidateConditions(ctx, w.e.D.DB.Pool, w.ws.ID, []RoutingCondition{{Field: "contact.field.seats", Op: "gt", Value: v}})
		var he *httpx.Error
		if !errors.As(err, &he) || he.Code != "validation_failed" || he.Status != 422 ||
			!strings.Contains(he.Detail, "field.seats") || !strings.Contains(he.Detail, "finite") {
			t.Fatalf("constructor %q: %v", v, err)
		}
	}

	// The direct API answers 422 validation_failed and stores nothing.
	var n int
	w.e.Scalar(`SELECT count(*) FROM routing_rules WHERE workspace_id = $1`, &n, w.ws.ID)
	if n != 0 {
		t.Fatalf("start with %d rules", n)
	}
	for _, v := range nonFinite {
		res := w.e.Do("POST", "/v1/routing-rules", w.admin(), map[string]any{"name": "bad " + v, "team_id": w.billing,
			"conditions": []map[string]any{{"field": "contact.field.seats", "op": "gt", "value": v}}})
		if res.Status != 422 || !strings.Contains(string(res.Body), "validation_failed") || !strings.Contains(string(res.Body), "field.seats") {
			t.Fatalf("POST %q: %d %s", v, res.Status, res.Body)
		}
	}
	w.e.Scalar(`SELECT count(*) FROM routing_rules WHERE workspace_id = $1`, &n, w.ws.ID)
	if n != 0 {
		t.Fatalf("%d invalid rules stored", n)
	}

	// Finite operands, including negatives, decimals, exponent and very large
	// finite numbers, plus set/not_set, are kept as sent.
	var r RoutingRule
	w.e.Do("POST", "/v1/routing-rules", w.admin(), map[string]any{"name": "Seats", "team_id": w.billing,
		"conditions": []map[string]any{
			{"field": "contact.field.seats", "op": "gte", "value": "-1.5"},
			{"field": "contact.field.seats", "op": "eq", "value": "1e3"},
			{"field": "contact.field.seats", "op": "lt", "value": "1e308"},
			{"field": "contact.field.seats", "op": "not_set", "value": ""},
		}}).Want(t, 201).JSON(t, &r)
	want := []struct{ op, value string }{
		{"gte", "-1.5"}, {"eq", "1e3"}, {"lt", "1e308"}, {"not_set", ""},
	}
	if len(r.Conditions) != len(want) {
		t.Fatalf("stored %+v", r.Conditions)
	}
	for i, c := range want {
		if r.Conditions[i].Op != c.op || r.Conditions[i].Value != c.value {
			t.Fatalf("condition %d = %+v, want %+v", i, r.Conditions[i], c)
		}
	}

	// A bad operand on PATCH is refused and leaves the stored rule unchanged.
	w.e.Do("PATCH", "/v1/routing-rules/"+r.ID.String(), w.admin(), map[string]any{
		"conditions": []map[string]any{{"field": "contact.field.seats", "op": "gt", "value": "Infinity"}}}).Want(t, 422)
	var kept RoutingRule
	w.e.Do("GET", "/v1/routing-rules/"+r.ID.String(), w.admin(), nil).Want(t, 200).JSON(t, &kept)
	if len(kept.Conditions) != 4 || kept.Conditions[1].Value != "1e3" {
		t.Fatalf("rule changed after failed patch: %+v", kept.Conditions)
	}
}
