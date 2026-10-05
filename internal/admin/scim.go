package admin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// SCIM 2.0 (RFC 7643, RFC 7644): the parts of the protocol that are pure parsing — request
// bodies, filters and PATCH operations — kept here so they are tested without a server.
// internal/api/scim.go serves them.

// SCIM schema URNs.
const (
	SchemaUser     = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaGroup    = "urn:ietf:params:scim:schemas:core:2.0:Group"
	SchemaList     = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SchemaPatchOp  = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
	SchemaError    = "urn:ietf:params:scim:api:messages:2.0:Error"
	SchemaSPConfig = "urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"
	SchemaResType  = "urn:ietf:params:scim:schemas:core:2.0:ResourceType"
	SchemaSchema   = "urn:ietf:params:scim:schemas:core:2.0:Schema"
)

// SCIMError is a protocol error: an HTTP status and, for 400s, a scimType (RFC 7644 §3.12).
type SCIMError struct {
	Status   int
	SCIMType string
	Detail   string
}

func (e *SCIMError) Error() string { return e.Detail }

func scimBad(scimType, format string, args ...any) error {
	return &SCIMError{Status: 400, SCIMType: scimType, Detail: fmt.Sprintf(format, args...)}
}

// SCIMUser is a User resource as an identity provider sends it. Attributes Conductor does
// not keep (addresses, titles, phone numbers, roles, enterprise extensions) are accepted and
// ignored — and so a provider can never grant a role through SCIM.
type SCIMUser struct {
	Schemas    []string `json:"schemas"`
	UserName   string   `json:"userName"`
	ExternalID string   `json:"externalId"`
	Name       struct {
		Formatted  string `json:"formatted"`
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"name"`
	DisplayName string      `json:"displayName"`
	Emails      []SCIMEmail `json:"emails"`
	Active      *SCIMBool   `json:"active"`
}

// SCIMEmail is one entry of a user's emails.
type SCIMEmail struct {
	Value   string   `json:"value"`
	Type    string   `json:"type,omitempty"`
	Primary SCIMBool `json:"primary,omitempty"`
}

// SCIMBool is a SCIM boolean. Microsoft Entra ID sends PATCH values as the strings "True"
// and "False"; RFC 7643 says boolean. Both are accepted.
type SCIMBool bool

func (b *SCIMBool) UnmarshalJSON(raw []byte) error {
	var v bool
	if err := json.Unmarshal(raw, &v); err == nil {
		*b = SCIMBool(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			*b = true
			return nil
		case "false":
			*b = false
			return nil
		}
	}
	return fmt.Errorf("%s is not a boolean", raw)
}

// ParseSCIMUser reads and checks a User body.
func ParseSCIMUser(body []byte) (SCIMUser, error) {
	var u SCIMUser
	if err := json.Unmarshal(body, &u); err != nil {
		return u, scimBad("invalidSyntax", "the body is not a SCIM User: %v", err)
	}
	u.UserName = strings.TrimSpace(u.UserName)
	if u.UserName == "" || len(u.UserName) > 254 {
		return u, scimBad("invalidValue", "userName is required (at most 254 characters)")
	}
	return u, nil
}

// PrimaryEmail is the address single sign-on links the account by: the primary email, else
// the first, else the userName when it is an address.
func (u SCIMUser) PrimaryEmail() string {
	for _, e := range u.Emails {
		if bool(e.Primary) && plausibleEmail(e.Value) {
			return strings.ToLower(strings.TrimSpace(e.Value))
		}
	}
	for _, e := range u.Emails {
		if plausibleEmail(e.Value) {
			return strings.ToLower(strings.TrimSpace(e.Value))
		}
	}
	if plausibleEmail(u.UserName) {
		return strings.ToLower(u.UserName)
	}
	return ""
}

// FullName is the best display name the body offers.
func (u SCIMUser) FullName() string {
	switch {
	case strings.TrimSpace(u.DisplayName) != "":
		return strings.TrimSpace(u.DisplayName)
	case strings.TrimSpace(u.Name.Formatted) != "":
		return strings.TrimSpace(u.Name.Formatted)
	}
	return strings.TrimSpace(u.Name.GivenName + " " + u.Name.FamilyName)
}

func plausibleEmail(s string) bool {
	s = strings.TrimSpace(s)
	local, d, ok := strings.Cut(s, "@")
	return ok && local != "" && strings.Contains(d, ".") && !strings.ContainsAny(s, " \t\r\n<>,;\"") &&
		!strings.Contains(d, "@") && len(s) <= 254
}

// SCIMFilter is the one filter form identity providers send: attribute eq "value".
type SCIMFilter struct {
	Attribute string // lowercased: username, externalid, displayname, id, emails.value
	Value     string
}

var filterPattern = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z0-9.]*)\s+(?i:eq)\s+"((?:[^"\\]|\\.)*)"\s*$`)

// ParseSCIMFilter parses a filter, allowing only `eq` on the given attributes. An empty
// filter is nil.
func ParseSCIMFilter(raw string, allowed ...string) (*SCIMFilter, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	m := filterPattern.FindStringSubmatch(raw)
	if m == nil {
		return nil, scimBad("invalidFilter", "only filters of the form attribute eq \"value\" are supported")
	}
	attr := strings.ToLower(m[1])
	ok := false
	for _, a := range allowed {
		ok = ok || strings.ToLower(a) == attr
	}
	if !ok {
		return nil, scimBad("invalidFilter", "filtering on %s is not supported (supported: %s)", m[1], strings.Join(allowed, ", "))
	}
	var value string
	if err := json.Unmarshal([]byte(`"`+m[2]+`"`), &value); err != nil {
		return nil, scimBad("invalidFilter", "the filter value is not a valid string")
	}
	return &SCIMFilter{Attribute: attr, Value: value}, nil
}

// patchRequest is a PatchOp body.
type patchRequest struct {
	Schemas    []string `json:"schemas"`
	Operations []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	} `json:"Operations"`
}

func parsePatch(body []byte) (patchRequest, error) {
	var req patchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return req, scimBad("invalidSyntax", "the body is not a SCIM PatchOp: %v", err)
	}
	if len(req.Operations) == 0 {
		return req, scimBad("invalidSyntax", "a PatchOp needs Operations")
	}
	for i := range req.Operations {
		op := strings.ToLower(req.Operations[i].Op)
		if op != "add" && op != "replace" && op != "remove" {
			return req, scimBad("invalidSyntax", "unsupported op %q", req.Operations[i].Op)
		}
		req.Operations[i].Op = op
	}
	return req, nil
}

// UserPatch is what a PATCH on a User changes. Attributes not listed are accepted and
// ignored.
type UserPatch struct {
	Active      *bool
	UserName    *string
	ExternalID  *string
	DisplayName *string
	GivenName   *string
	FamilyName  *string
	Email       *string
}

// ParseUserPatch reads a PatchOp body for a User, in the shapes Okta and Entra send:
//
//	{"op":"replace","value":{"active":false}}                 (Okta)
//	{"op":"Replace","path":"active","value":"False"}          (Entra)
//	{"op":"Add","path":"emails[type eq \"work\"].value","value":"a@b.c"}
func ParseUserPatch(body []byte) (UserPatch, error) {
	var out UserPatch
	req, err := parsePatch(body)
	if err != nil {
		return out, err
	}
	for _, op := range req.Operations {
		if op.Path == "" {
			if op.Op == "remove" {
				return out, scimBad("noTarget", "remove needs a path")
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(op.Value, &fields); err != nil {
				return out, scimBad("invalidValue", "a patch without a path needs an object value")
			}
			for k, v := range fields {
				if err := out.set(k, v); err != nil {
					return out, err
				}
			}
			continue
		}
		if op.Op == "remove" {
			// Removing an attribute Conductor keeps clears it, except active and userName,
			// which an account cannot be without.
			switch strings.ToLower(op.Path) {
			case "active", "username":
				return out, scimBad("mutability", "%s cannot be removed", op.Path)
			}
			if err := out.set(op.Path, json.RawMessage(`""`)); err != nil {
				return out, err
			}
			continue
		}
		if err := out.set(op.Path, op.Value); err != nil {
			return out, err
		}
	}
	return out, nil
}

var emailPath = regexp.MustCompile(`(?i)^emails(\[.*\])?(\.value)?$`)

func (u *UserPatch) set(path string, raw json.RawMessage) error {
	str := func(dst **string) error {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return scimBad("invalidValue", "%s must be a string", path)
		}
		s = strings.TrimSpace(s)
		*dst = &s
		return nil
	}
	p := strings.ToLower(strings.TrimPrefix(strings.ToLower(path), strings.ToLower(SchemaUser)+":"))
	switch {
	case p == "active":
		var b SCIMBool
		if err := json.Unmarshal(raw, &b); err != nil {
			return scimBad("invalidValue", "active must be a boolean")
		}
		v := bool(b)
		u.Active = &v
	case p == "username":
		return str(&u.UserName)
	case p == "externalid":
		return str(&u.ExternalID)
	case p == "displayname":
		return str(&u.DisplayName)
	case p == "name.givenname":
		return str(&u.GivenName)
	case p == "name.familyname":
		return str(&u.FamilyName)
	case p == "name":
		var n struct {
			GivenName  *string `json:"givenName"`
			FamilyName *string `json:"familyName"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return scimBad("invalidValue", "name must be an object")
		}
		u.GivenName, u.FamilyName = n.GivenName, n.FamilyName
	case emailPath.MatchString(p):
		// A single address (emails[type eq "work"].value) or the whole list.
		var s string
		if json.Unmarshal(raw, &s) == nil {
			s = strings.ToLower(strings.TrimSpace(s))
			u.Email = &s
			return nil
		}
		var list []SCIMEmail
		if err := json.Unmarshal(raw, &list); err != nil {
			return scimBad("invalidValue", "emails must be a list of addresses")
		}
		email := SCIMUser{Emails: list}.PrimaryEmail()
		u.Email = &email
	}
	return nil
}

// GroupPatch is what a PATCH on a Group changes.
type GroupPatch struct {
	DisplayName *string
	ExternalID  *string
	// Add and Remove are member ids; ReplaceMembers, when set, is the whole new list.
	Add, Remove    []string
	ReplaceMembers *[]string
}

var memberFilterPath = regexp.MustCompile(`(?i)^members\[\s*value\s+eq\s+"([^"]+)"\s*\]$`)

type memberRef struct {
	Value string `json:"value"`
}

// ParseGroupPatch reads a PatchOp body for a Group, in the shapes Okta and Entra send:
//
//	{"op":"add","path":"members","value":[{"value":"<id>"}]}
//	{"op":"remove","path":"members[value eq \"<id>\"]"}          (Okta)
//	{"op":"Remove","path":"members","value":[{"value":"<id>"}]}  (Entra)
//	{"op":"replace","value":{"displayName":"Engineering"}}
func ParseGroupPatch(body []byte) (GroupPatch, error) {
	var out GroupPatch
	req, err := parsePatch(body)
	if err != nil {
		return out, err
	}
	members := func(raw json.RawMessage) ([]string, error) {
		var refs []memberRef
		if err := json.Unmarshal(raw, &refs); err != nil {
			var one memberRef
			if err := json.Unmarshal(raw, &one); err != nil {
				return nil, scimBad("invalidValue", "members must be a list of {\"value\": id}")
			}
			refs = []memberRef{one}
		}
		ids := make([]string, 0, len(refs))
		for _, r := range refs {
			if r.Value != "" {
				ids = append(ids, r.Value)
			}
		}
		return ids, nil
	}
	for _, op := range req.Operations {
		path := strings.TrimSpace(op.Path)
		switch {
		case path == "":
			var fields struct {
				DisplayName *string         `json:"displayName"`
				ExternalID  *string         `json:"externalId"`
				Members     json.RawMessage `json:"members"`
			}
			if err := json.Unmarshal(op.Value, &fields); err != nil {
				return out, scimBad("invalidValue", "a patch without a path needs an object value")
			}
			if fields.DisplayName != nil {
				out.DisplayName = fields.DisplayName
			}
			if fields.ExternalID != nil {
				out.ExternalID = fields.ExternalID
			}
			if len(fields.Members) > 0 {
				ids, err := members(fields.Members)
				if err != nil {
					return out, err
				}
				if op.Op == "add" {
					out.Add = append(out.Add, ids...)
				} else {
					out.ReplaceMembers = &ids
				}
			}
		case strings.EqualFold(path, "displayName"):
			var s string
			if err := json.Unmarshal(op.Value, &s); err != nil {
				return out, scimBad("invalidValue", "displayName must be a string")
			}
			out.DisplayName = &s
		case strings.EqualFold(path, "externalId"):
			var s string
			_ = json.Unmarshal(op.Value, &s)
			out.ExternalID = &s
		case strings.EqualFold(path, "members"):
			var ids []string
			if len(op.Value) > 0 && string(op.Value) != "null" {
				if ids, err = members(op.Value); err != nil {
					return out, err
				}
			}
			switch op.Op {
			case "add":
				out.Add = append(out.Add, ids...)
			case "remove":
				if len(op.Value) == 0 {
					// Removing the attribute itself empties the group.
					empty := []string{}
					out.ReplaceMembers = &empty
				}
				out.Remove = append(out.Remove, ids...)
			case "replace":
				out.ReplaceMembers = &ids
			}
		case memberFilterPath.MatchString(path):
			if op.Op != "remove" {
				return out, scimBad("invalidPath", "only remove is supported on %s", path)
			}
			out.Remove = append(out.Remove, memberFilterPath.FindStringSubmatch(path)[1])
		default:
			return out, scimBad("invalidPath", "unsupported path %q", path)
		}
	}
	return out, nil
}

// SCIMGroup is a Group resource as a provider sends it.
type SCIMGroup struct {
	Schemas     []string    `json:"schemas"`
	DisplayName string      `json:"displayName"`
	ExternalID  string      `json:"externalId"`
	Members     []memberRef `json:"members"`
}

// ParseSCIMGroup reads and checks a Group body.
func ParseSCIMGroup(body []byte) (SCIMGroup, []string, error) {
	var g SCIMGroup
	if err := json.Unmarshal(body, &g); err != nil {
		return g, nil, scimBad("invalidSyntax", "the body is not a SCIM Group: %v", err)
	}
	g.DisplayName = strings.TrimSpace(g.DisplayName)
	if g.DisplayName == "" || len(g.DisplayName) > 256 {
		return g, nil, scimBad("invalidValue", "displayName is required (at most 256 characters)")
	}
	ids := []string{}
	for _, m := range g.Members {
		if m.Value != "" {
			ids = append(ids, m.Value)
		}
	}
	return g, ids, nil
}
