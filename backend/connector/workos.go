package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var workosKeyRE = regexp.MustCompile(`^sk_[A-Za-z0-9_]+$`)

var workosObjects = []saasTable{
	{"organizations", "organization_id"},
	{"users", "user_id"},
	{"organization_memberships", "organization_membership_id"},
	{"invitations", "invitation_id"},
	{"sso_connections", "connection_id"},
	{"directories", "directory_id"},
	{"directory_users", "directory_user_id"},
	{"directory_groups", "directory_group_id"},
	// Membership rows; listed per group, so the key is the (group, user) pair set in Discover.
	{"directory_group_members", "directory_user_id"},
}

// workosListPaths proves an object readable; per-parent lists depend on their parent's list.
// WorkOS refuses /directory_groups without a directory or user (422), so groups
// and their members are proved through /directories.
var workosListPaths = map[string]string{
	"organizations":            "/organizations",
	"users":                    "/user_management/users",
	"organization_memberships": "/organizations",
	"invitations":              "/user_management/invitations",
	"sso_connections":          "/connections",
	"directories":              "/directories",
	"directory_users":          "/directories",
	"directory_groups":         "/directories",
	"directory_group_members":  "/directories",
}

// workosEventsProbe proves the key reads events; WorkOS requires a type filter.
const workosEventsProbe = "/events?events=organization.created"

var workosForeignKeys = referencedBy(workosObjects, map[string][]string{
	"organization_memberships": {"organization_id", "user_id"},
	"invitations":              {"organization_id"},
	"sso_connections":          {"organization_id"},
	"directories":              {"organization_id"},
	"directory_users":          {"directory_id", "organization_id"},
	"directory_groups":         {"directory_id", "organization_id"},
	"directory_group_members":  {"directory_group_id", "directory_user_id"},
})

type workosKind struct {
	base string
	http *http.Client
}

// workosSettings are what Discover learns; the customer sets nothing but the key.
type workosSettings struct {
	// Events is whether the key can read events, so a change sync can follow them.
	Events bool `json:"events,omitempty"`
}

func (k *workosKind) Name() string        { return "workos" }
func (k *workosKind) DisplayName() string { return "WorkOS" }

func (k *workosKind) Prepare(src Source) (json.RawMessage, error) {
	if !workosKeyRE.MatchString(strings.TrimSpace(src.Secret)) {
		return nil, invalid("enter a WorkOS API key (sk_…)")
	}
	if err := emptySettings(src.Settings); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

// Discover lists the readable objects and records in the settings whether the
// key reads events. A key that can't still syncs every table in full.
func (k *workosKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	header := workosHeader(src)
	paths := append(uniqueValues(workosListPaths), workosEventsProbe)
	readable, err := probeAll(ctx, paths, func(ctx context.Context, path string) (bool, error) {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		status, _, err := get(ctx, k.http, k.base+path+sep+"limit=1", header)
		switch {
		case err != nil:
			return false, refused(err, "could not reach WorkOS; try again")
		case status == http.StatusUnauthorized:
			return false, refused(nil, "WorkOS rejected this key; check it and test again")
		case status == http.StatusForbidden || status == http.StatusNotFound:
			return false, nil
		case status >= 300:
			return false, refused(nil, "WorkOS returned HTTP %d; try again", status)
		}
		return true, nil
	})
	if err != nil {
		return Discovery{}, err
	}
	d := saasDiscovery(workosObjects, func(name string) bool { return readable[workosListPaths[name]] }, workosForeignKeys)
	if len(d.Tables) == 0 {
		return Discovery{}, refused(nil, "this key cannot read any supported WorkOS objects; check the key and test again")
	}
	for i := range d.Tables {
		if d.Tables[i].Name == "directory_group_members" {
			d.Tables[i].PrimaryKey = []string{"directory_group_id", "directory_user_id"}
		}
	}
	if !readable[workosEventsProbe] {
		d.Warnings = append(d.Warnings, "this key can't read events, so on Pro and Max every sync reads every object in full; "+
			"use a key that can read the Events API to read only what changed")
	}
	if d.Settings, err = json.Marshal(workosSettings{Events: readable[workosEventsProbe]}); err != nil {
		return Discovery{}, fmt.Errorf("encode workos settings: %w", err)
	}
	return d, nil
}

func (k *workosKind) Hint(secret string) string { return keyHint(strings.TrimSpace(secret), 2) }

func workosHeader(src Source) http.Header {
	return http.Header{"Authorization": {"Bearer " + strings.TrimSpace(src.Secret)}}
}

// maxWorkOSPages stops a runaway list.
const maxWorkOSPages = 20_000

// workosObjectPaths is the list endpoint each object's rows come from.
var workosObjectPaths = map[string]string{
	"organizations":            "/organizations",
	"users":                    "/user_management/users",
	"organization_memberships": "/user_management/organization_memberships",
	"invitations":              "/user_management/invitations",
	"sso_connections":          "/connections",
	"directories":              "/directories",
	"directory_users":          "/directory_users",
	"directory_groups":         "/directory_groups",
	"directory_group_members":  "/directory_users",
}

// workosParent is how a per-parent object is listed: for each row of path,
// list the object with param set to the parent's id, stored as column. A
// parent that is itself per-parent names its own parent in within.
type workosParent struct {
	path, param, column string
	within              *workosParent
}

var workosParents = map[string]workosParent{
	"organization_memberships": {path: "/organizations", param: "organization_id", column: "organization_id"},
	"directory_users":          {path: "/directories", param: "directory", column: "directory_id"},
	"directory_groups":         {path: "/directories", param: "directory", column: "directory_id"},
	"directory_group_members": {path: "/directory_groups", param: "group", column: "directory_group_id",
		within: &workosParent{path: "/directories", param: "directory"}},
}

func (k *workosKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return table, k.copyRows(ctx, src, table.Name, w)
}

// workosObject is the object a table holds; ok is false for a table WorkOS doesn't have.
func workosObject(table string) (saasTable, bool) {
	for _, o := range workosObjects {
		if o.name == table {
			return o, true
		}
	}
	return saasTable{}, false
}

func (k *workosKind) copyRows(ctx context.Context, src Source, table string, w RowWriter) error {
	object, ok := workosObject(table)
	if !ok {
		return invalid("WorkOS has no object %q", table)
	}
	header := workosHeader(src)
	path := workosObjectPaths[table]
	parent, perParent := workosParents[table]
	if !perParent {
		return k.list(ctx, header, path, nil, func(row map[string]any) error {
			return w.Write(normalizeWorkOS(object, row, workosParent{}, ""))
		})
	}
	return k.listParents(ctx, header, parent, func(p map[string]any) error {
		parentID, _ := p["id"].(string)
		return k.list(ctx, header, path, url.Values{parent.param: {parentID}}, func(row map[string]any) error {
			return w.Write(normalizeWorkOS(object, row, parent, parentID))
		})
	})
}

// listParents lists every row of parent.path, under each row of parent.within when set.
func (k *workosKind) listParents(ctx context.Context, header http.Header, parent workosParent, each func(map[string]any) error) error {
	if parent.within == nil {
		return k.list(ctx, header, parent.path, nil, each)
	}
	return k.list(ctx, header, parent.within.path, nil, func(outer map[string]any) error {
		outerID, _ := outer["id"].(string)
		return k.list(ctx, header, parent.path, url.Values{parent.within.param: {outerID}}, each)
	})
}

// get sends one GET of path with query, retrying rate limits.
func (k *workosKind) get(ctx context.Context, header http.Header, path string, query url.Values) (int, []byte, error) {
	target := k.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return fetch(ctx, k.http, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err == nil {
			req.Header = header.Clone()
		}
		return req, err
	})
}

// workosRefusal is the error for a read of path that failed; nil when it succeeded.
func workosRefusal(status int, err error, path string) error {
	switch {
	case err != nil:
		return refused(err, "could not reach WorkOS; the sync will retry")
	case status == http.StatusUnauthorized:
		return refused(nil, "WorkOS rejected this key; update the connector")
	case status == http.StatusForbidden || status == http.StatusNotFound:
		return refused(nil, "this key can no longer read %s; check the WorkOS environment", path)
	case status >= 300:
		return refused(nil, "WorkOS returned HTTP %d for %s", status, path)
	}
	return nil
}

// list pages through a WorkOS list endpoint in creation order.
func (k *workosKind) list(ctx context.Context, header http.Header, path string, filter url.Values, each func(map[string]any) error) error {
	after := ""
	for page := 0; ; page++ {
		if page == maxWorkOSPages {
			return fmt.Errorf("workos %s: more than %d pages", path, maxWorkOSPages)
		}
		query := url.Values{"limit": {"100"}, "order": {"asc"}}
		for key, values := range filter {
			query[key] = values
		}
		if after != "" {
			query.Set("after", after)
		}
		status, body, err := k.get(ctx, header, path, query)
		if err := workosRefusal(status, err, path); err != nil {
			return err
		}
		var listPage struct {
			Data         []map[string]any `json:"data"`
			ListMetadata struct {
				After string `json:"after"`
			} `json:"list_metadata"`
		}
		if err := json.Unmarshal(body, &listPage); err != nil {
			return fmt.Errorf("decode workos %s page: %w", path, err)
		}
		for _, row := range listPage.Data {
			if err := each(row); err != nil {
				return err
			}
		}
		after = listPage.ListMetadata.After
		if after == "" || len(listPage.Data) == 0 {
			return nil
		}
	}
}

// normalizeWorkOS renames id to the object's key and adds the parent's id.
// Group membership rows keep only the (group, user) pair.
func normalizeWorkOS(object saasTable, row map[string]any, parent workosParent, parentID string) map[string]any {
	if object.name == "directory_group_members" {
		return map[string]any{parent.column: parentID, "directory_user_id": row["id"]}
	}
	out := make(map[string]any, len(row)+1)
	if parent.column != "" {
		out[parent.column] = parentID
	}
	for field, value := range row {
		switch {
		case field == "object", field == "groups" && object.name == "directory_users":
		case field == "id":
			out[object.key] = value
		default:
			out[field] = value
		}
	}
	return out
}
