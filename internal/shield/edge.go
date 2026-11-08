package shield

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The blocks are enforced at Railway's edge, before a request reaches the
// bot: the service's edge rules get two rules the bot owns, one that blocks the
// banned addresses and one that challenges the others. Railway has no rules
// that run out, so the bot rewrites both whenever the set changes, including
// when a block ends. Every other rule in the service's ruleset is kept as it
// is; the bot's rules go first, so a ban beats any allow rule below it.
//
// The edge matches client addresses on IPv4 only. IPv6 bans are refused by the
// bot itself; IPv6 challenges have no effect.

const (
	// railwayAPI is Railway's public GraphQL endpoint.
	railwayAPI = "https://backboard.railway.com/graphql/v2"

	// ownRule starts the description of the rules the bot writes; any rule so
	// described is the bot's to replace.
	ownRule = "steki-shield"

	// Limits Railway puts on one rule: 64 values per "in" clause and 32
	// clauses. maxEdgeIPs keeps both rules well inside the 64 KiB ruleset.
	maxInValues = 64
	maxEdgeIPs  = 16 * maxInValues

	// ownPriorities are the priorities of the bot's two rules, which must sort
	// before everything else.
	banPriority       = 1
	challengePriority = 2

	banBody = "Η πρόσβαση από αυτή τη σύνδεση έχει μπλοκαριστεί προσωρινά. Δοκίμασε ξανά αργότερα.\n"
)

// RailwayEdge writes the blocks to a Railway service's edge rules.
type RailwayEdge struct {
	token, serviceID, environmentID string
	api                             string
	http                            *http.Client
}

// NewRailwayEdge returns an edge for the service in the environment, reached
// with a project token.
func NewRailwayEdge(token, serviceID, environmentID string) *RailwayEdge {
	return &RailwayEdge{
		token: token, serviceID: serviceID, environmentID: environmentID,
		api: railwayAPI, http: &http.Client{Timeout: 20 * time.Second},
	}
}

// Sync makes the bot's edge rules challenge and block exactly the given IPv4
// addresses, leaving every other rule as it is. Nothing is written when the
// rules already match.
func (e *RailwayEdge) Sync(ctx context.Context, challenged, banned []string) error {
	var cur struct {
		ServiceInstance *struct {
			EdgeConfig *struct {
				EdgeRules json.RawMessage `json:"edgeRules"`
			} `json:"edgeConfig"`
		} `json:"serviceInstance"`
	}
	err := e.call(ctx, `query($s: String!, $e: String!) {
		serviceInstance(serviceId: $s, environmentId: $e) { edgeConfig { edgeRules } } }`,
		map[string]any{"s": e.serviceID, "e": e.environmentID}, &cur)
	if err != nil {
		return fmt.Errorf("read edge rules: %w", err)
	}
	if cur.ServiceInstance == nil {
		return errors.New("read edge rules: no such service instance")
	}
	var old json.RawMessage
	if cur.ServiceInstance.EdgeConfig != nil {
		old = cur.ServiceInstance.EdgeConfig.EdgeRules
	}
	next, changed, err := mergeRules(old, challenged, banned)
	if err != nil || !changed {
		return err
	}
	err = e.call(ctx, `mutation($input: UpdateServiceEdgeRulesInput!) {
		updateServiceEdgeRules(input: $input) { id } }`,
		map[string]any{"input": map[string]any{
			"serviceId": e.serviceID, "environmentId": e.environmentID, "edgeRules": next,
		}}, nil)
	if err != nil {
		return fmt.Errorf("write edge rules: %w", err)
	}
	return nil
}

// call runs one GraphQL operation and decodes its data into out.
func (e *RailwayEdge) call(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.api, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Project-Access-Token", e.token)
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("railway answered %d", resp.StatusCode)
	}
	if len(res.Errors) > 0 {
		msgs := make([]string, len(res.Errors))
		for i, m := range res.Errors {
			msgs[i] = m.Message
		}
		return fmt.Errorf("railway: %s", strings.Join(msgs, "; "))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("railway answered %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(res.Data, out)
}

// mergeRules returns the ruleset old with the bot's rules replaced by ones for
// challenged and banned, and whether that differs from old. Rules that are not
// the bot's keep their fields and order; they are moved down only when their
// priorities would clash with the bot's.
func mergeRules(old json.RawMessage, challenged, banned []string) (map[string]any, bool, error) {
	set := map[string]any{}
	if len(old) > 0 && string(old) != "null" {
		dec := json.NewDecoder(bytes.NewReader(old))
		dec.UseNumber()
		if err := dec.Decode(&set); err != nil {
			return nil, false, fmt.Errorf("edge rules are not a JSON object: %w", err)
		}
	}
	rawRules, _ := set["rules"].([]any)
	var others, oldOwn []map[string]any
	for _, r := range rawRules {
		rule, ok := r.(map[string]any)
		if !ok {
			return nil, false, errors.New("edge rules hold a rule that is not an object")
		}
		if d, _ := rule["description"].(string); strings.HasPrefix(d, ownRule) {
			oldOwn = append(oldOwn, rule)
			continue
		}
		others = append(others, rule)
	}

	// Move the others down past the bot's priorities if any sits there.
	low := int64(-1)
	for _, r := range others {
		p, err := priority(r)
		if err != nil {
			return nil, false, err
		}
		if low < 0 || p < low {
			low = p
		}
	}
	shifted := low >= 0 && low <= challengePriority
	if shifted {
		shift := challengePriority + 1 - low
		for _, r := range others {
			p, _ := priority(r)
			r["priority"] = p + shift
		}
	}

	var own []map[string]any
	if len(banned) > 0 {
		own = append(own, map[string]any{
			"description": ownRule + ": banned addresses (written by the bot; edits are overwritten)",
			"priority":    banPriority,
			"if":          ipCondition(banned),
			"then":        map[string]any{"action": "block", "params": map[string]any{"status": 403, "body": banBody}},
		})
	}
	if len(challenged) > 0 {
		own = append(own, map[string]any{
			"description": ownRule + ": challenged addresses (written by the bot; edits are overwritten)",
			"priority":    challengePriority,
			"if":          ipCondition(challenged),
			"then":        map[string]any{"action": "challenge"},
		})
	}
	changed := shifted || !sameRules(oldOwn, own)
	// Railway gave the bot's rules ids; a rule that stays keeps its id.
	for _, r := range own {
		for _, o := range oldOwn {
			if o["priority"] != nil && fmt.Sprint(o["priority"]) == fmt.Sprint(r["priority"]) && o["id"] != nil {
				r["id"] = o["id"]
			}
		}
	}
	rules := make([]any, 0, len(own)+len(others))
	for _, r := range own {
		rules = append(rules, r)
	}
	for _, r := range others {
		rules = append(rules, r)
	}
	set["version"] = 1
	set["rules"] = rules
	return set, changed, nil
}

// priority reads a rule's priority.
func priority(r map[string]any) (int64, error) {
	n, ok := r["priority"].(json.Number)
	if !ok {
		return 0, errors.New("an edge rule has no numeric priority")
	}
	return n.Int64()
}

// ipCondition matches a request from any of ips, in as many "in" clauses as
// Railway's limit on values per clause needs.
func ipCondition(ips []string) map[string]any {
	var clauses []any
	for len(ips) > 0 {
		n := min(len(ips), maxInValues)
		clauses = append(clauses, map[string]any{"attr": "ipv4.src", "op": "in", "value": ips[:n]})
		ips = ips[n:]
	}
	if len(clauses) == 1 {
		return clauses[0].(map[string]any)
	}
	return map[string]any{"or": clauses}
}

// sameRules reports whether two lists of the bot's rules say the same thing,
// whatever ids and defaults Railway added to the stored ones.
func sameRules(a, b []map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if enabled, ok := a[i]["enabled"].(bool); ok && !enabled {
			return false
		}
		for _, k := range []string{"description", "priority", "if", "then"} {
			x, err1 := json.Marshal(a[i][k])
			y, err2 := json.Marshal(b[i][k])
			if err1 != nil || err2 != nil || !bytes.Equal(x, y) {
				return false
			}
		}
	}
	return true
}
