package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var linearKeyRE = regexp.MustCompile(`^lin_api_[A-Za-z0-9]+$`)

const (
	linearPageSize = 100
	maxLinearPages = 20_000
)

var linearObjects = []saasTable{
	{"teams", "team_id"},
	{"users", "user_id"},
	{"projects", "project_id"},
	{"cycles", "cycle_id"},
	{"workflow_states", "workflow_state_id"},
	{"issue_labels", "label_id"},
	{"issues", "issue_id"},
	{"comments", "comment_id"},
}

// linearQueries is each table's root connection and the node fields it reads;
// a nested object is read as its id alone, which keeps a page's complexity low.
var linearQueries = map[string]struct{ root, fields string }{
	"teams": {"teams", "id key name description timezone createdAt updatedAt archivedAt parent { id }"},
	"users": {"users", "id name displayName email active admin guest title timezone createdAt updatedAt archivedAt"},
	"projects": {"projects", "id name description url priority progress startDate targetDate startedAt completedAt " +
		"canceledAt createdAt updatedAt archivedAt status { name type } lead { id } creator { id }"},
	"cycles":          {"cycles", "id number name description startsAt endsAt completedAt progress createdAt updatedAt archivedAt team { id }"},
	"workflow_states": {"workflowStates", "id name type position color description createdAt updatedAt archivedAt team { id }"},
	"issue_labels": {"issueLabels", "id name color description isGroup createdAt updatedAt archivedAt " +
		"team { id } parent { id } creator { id }"},
	"issues": {"issues", "id identifier number title description priority estimate url dueDate labelIds createdAt " +
		"updatedAt startedAt completedAt canceledAt archivedAt team { id } state { id } assignee { id } creator { id } " +
		"project { id } cycle { id } parent { id }"},
	"comments": {"comments", "id body url createdAt updatedAt archivedAt issue { id } user { id } parent { id }"},
}

// linearReferences is the column a nested object's id lands in; parent always
// points at a row of the same table.
var linearReferences = map[string]string{
	"team": "team_id", "state": "workflow_state_id", "assignee": "assignee_id", "creator": "creator_id",
	"project": "project_id", "cycle": "cycle_id", "lead": "lead_id", "issue": "issue_id", "user": "user_id",
	"parent": "parent_id",
}

var linearForeignKeys = map[string][]ForeignKey{
	"teams":           {foreignKey("parent_id", "teams", "team_id")},
	"projects":        {foreignKey("lead_id", "users", "user_id"), foreignKey("creator_id", "users", "user_id")},
	"cycles":          {foreignKey("team_id", "teams", "team_id")},
	"workflow_states": {foreignKey("team_id", "teams", "team_id")},
	"issue_labels": {foreignKey("team_id", "teams", "team_id"), foreignKey("parent_id", "issue_labels", "label_id"),
		foreignKey("creator_id", "users", "user_id")},
	"issues": {foreignKey("team_id", "teams", "team_id"), foreignKey("workflow_state_id", "workflow_states", "workflow_state_id"),
		foreignKey("assignee_id", "users", "user_id"), foreignKey("creator_id", "users", "user_id"),
		foreignKey("project_id", "projects", "project_id"), foreignKey("cycle_id", "cycles", "cycle_id"),
		foreignKey("parent_id", "issues", "issue_id")},
	"comments": {foreignKey("issue_id", "issues", "issue_id"), foreignKey("user_id", "users", "user_id"),
		foreignKey("parent_id", "comments", "comment_id")},
}

func foreignKey(column, table, referenced string) ForeignKey {
	return ForeignKey{Column: column, ReferencedTable: table, ReferencedColumn: referenced}
}

type linearKind struct {
	base string
	http *http.Client
}

func (k *linearKind) Name() string        { return "linear" }
func (k *linearKind) DisplayName() string { return "Linear" }

func (k *linearKind) Prepare(src Source) (json.RawMessage, error) {
	if !linearKeyRE.MatchString(strings.TrimSpace(src.Secret)) {
		return nil, invalid("enter a Linear personal API key (lin_api_…)")
	}
	if err := emptySettings(src.Settings); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

// Discover proves the key; a personal key reads everything its user can, so
// every object is readable once the key works.
func (k *linearKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	var viewer struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := k.query(ctx, src.Secret, `query { viewer { id } }`, nil, &viewer); err != nil {
		return Discovery{}, err
	}
	return saasDiscovery(linearObjects, func(string) bool { return true }, linearForeignKeys), nil
}

func (k *linearKind) Hint(secret string) string { return keyHint(strings.TrimSpace(secret), 2) }

// Sync pages a table's root connection in creation order, archived rows included.
func (k *linearKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return table, k.read(ctx, src.Secret, table.Name, nil, w.Write)
}

// read pages table's root connection, archived rows included, and writes each
// node normalized: every node in creation order, or with since set only those
// updated after it, in update order.
func (k *linearKind) read(ctx context.Context, key, table string, since *time.Time, write func(map[string]any) error) error {
	q, ok := linearQueries[table]
	if !ok {
		return invalid("Linear has no object %q", table)
	}
	var object saasTable
	for _, o := range linearObjects {
		if o.name == table {
			object = o
		}
	}
	params, args, variables := "$after: String", "orderBy: createdAt", map[string]any{}
	if since != nil {
		params += ", $since: DateTimeOrDuration!"
		args = "orderBy: updatedAt, filter: {updatedAt: {gt: $since}}"
		variables["since"] = since.UTC().Format(time.RFC3339)
	}
	query := fmt.Sprintf(`query(%s) { %s(first: %d, after: $after, includeArchived: true, %s) `+
		`{ nodes { %s } pageInfo { hasNextPage endCursor } } }`, params, q.root, linearPageSize, args, q.fields)
	var after any
	for page := 0; ; page++ {
		if page == maxLinearPages {
			return fmt.Errorf("linear %s: more than %d pages", q.root, maxLinearPages)
		}
		var data map[string]*struct {
			Nodes    []map[string]any `json:"nodes"`
			PageInfo *struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
		}
		variables["after"] = after
		if err := k.query(ctx, key, query, variables, &data); err != nil {
			return err
		}
		// An incomplete page fails the attempt; ending early would publish a partial table.
		connection := data[q.root]
		switch {
		case connection == nil || connection.PageInfo == nil:
			return fmt.Errorf("linear %s: the response has no %s page", q.root, q.root)
		case connection.PageInfo.HasNextPage && connection.PageInfo.EndCursor == "":
			return fmt.Errorf("linear %s: a page says more follow but gives no cursor", q.root)
		}
		for _, node := range connection.Nodes {
			if err := write(normalizeLinear(object, node)); err != nil {
				return err
			}
		}
		if !connection.PageInfo.HasNextPage {
			return nil
		}
		after = connection.PageInfo.EndCursor
	}
}

// query posts one GraphQL request and decodes its data into out. Linear
// reports a bad key and a spent rate limit as GraphQL errors.
func (k *linearKind) query(ctx context.Context, key, query string, variables map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	status, body, err := fetch(ctx, k.http, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, k.base+"/graphql", bytes.NewReader(payload))
		if err == nil {
			req.Header = http.Header{"Authorization": {strings.TrimSpace(key)}, "Content-Type": {"application/json"}}
		}
		return req, err
	})
	if err != nil {
		return refused(err, "could not reach Linear; try again")
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message    string `json:"message"`
			Extensions struct {
				Code string `json:"code"`
			} `json:"extensions"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(body, &resp) // a body that is not GraphQL is judged by its status below
	for _, e := range resp.Errors {
		switch e.Extensions.Code {
		case "AUTHENTICATION_ERROR":
			return refused(nil, "Linear rejected this key; check it and enter it again")
		case "RATELIMITED":
			return refused(nil, "this key has used up its Linear rate limit; try again later")
		}
	}
	switch {
	case status == http.StatusUnauthorized:
		return refused(nil, "Linear rejected this key; check it and enter it again")
	case len(resp.Errors) > 0:
		message := resp.Errors[0].Message
		if len(message) > 200 {
			message = message[:200]
		}
		return refused(nil, "Linear refused the request: %s", message)
	case status >= 300:
		return refused(nil, "Linear returned HTTP %d; try again", status)
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return fmt.Errorf("decode linear response: %w", err)
	}
	return nil
}

// normalizeLinear renames id to the table's key, keeps nested objects' ids
// and writes field names in snake_case.
func normalizeLinear(object saasTable, node map[string]any) map[string]any {
	out := make(map[string]any, len(node))
	for field, value := range node {
		column, isReference := linearReferences[field]
		switch {
		case field == "id":
			out[object.key] = value
		case field == "status":
			out["status_name"], out["status_type"] = nil, nil
			if s, ok := value.(map[string]any); ok {
				out["status_name"], out["status_type"] = s["name"], s["type"]
			}
		case isReference:
			out[column] = nil
			if ref, ok := value.(map[string]any); ok {
				out[column] = ref["id"]
			}
		default:
			out[snakeCase(field)] = value
		}
	}
	return out
}

// snakeCase turns a GraphQL field name such as labelIds into label_ids.
func snakeCase(field string) string {
	var b strings.Builder
	for i, r := range field {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
