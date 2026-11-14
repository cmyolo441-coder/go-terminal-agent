package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testPolicy = `{
  "version": 1,
  "default_roles": ["authenticated"],
  "roles": {
    "authenticated": {"permissions": ["session:read"]},
    "viewer":    {"permissions": ["repo:read", "tools:read_*"]},
    "developer": {"inherits": ["viewer"], "permissions": ["repo:write", "tools:browser_*", "tools:edit", "tools:shell"]},
    "admin":     {"inherits": ["developer"], "permissions": ["*"]},
    "auditor":   {"permissions": ["audit:*"]},
    "deployer":  {}
  },
  "group_mappings": [
    {"claim": "groups", "values": ["eng", "platform"], "roles": ["developer"]},
    {"claim": "realm_access.roles", "values": ["gta-admin"], "roles": ["admin"]},
    {"claim": "https://example.com/roles", "values": ["audit"], "roles": ["auditor"]},
    {"claim": "/resource_access/gta/roles", "values": ["viewer"], "roles": ["viewer"]}
  ],
  "rules": [
    {"id": "no-shell-in-prod", "effect": "deny", "permissions": ["tools:shell"], "conditions": {"environments": ["prod"]}},
    {"id": "protect-secrets", "effect": "deny", "permissions": ["repo:write"], "subjects": {"roles": ["*"]},
     "conditions": {"workspaces": ["/srv/secrets/**"]}},
    {"id": "bots-no-browser", "effect": "deny", "permissions": ["tools:browser_*"], "subjects": {"types": ["service_account"]}},
    {"id": "business-hours-deploy", "effect": "allow", "permissions": ["deploy:run"], "subjects": {"roles": ["developer"]},
     "conditions": {"time": {"days": ["mon","tue","wed","thu","fri"], "start": "09:00", "end": "17:00", "timezone": "UTC"}}},
    {"id": "night-batch", "effect": "allow", "permissions": ["batch:run"], "subjects": {"principals": ["svc-*"]},
     "conditions": {"time": {"start": "22:00", "end": "06:00"}}},
    {"id": "team-a-only", "effect": "allow", "permissions": ["reports:read"], "conditions": {"attributes": {"team": ["a"]}}}
  ]
}`

func testEngine(t testing.TB) *Engine {
	t.Helper()
	p, err := ParsePolicy([]byte(testPolicy))
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(p)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestAuthorizeTable(t *testing.T) {
	e := testEngine(t)
	// Wednesday 2026-11-11 12:00 UTC and Saturday 2026-11-14 12:00 UTC.
	wed := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	sat := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	night := time.Date(2026, 11, 11, 23, 30, 0, 0, time.UTC)
	early := time.Date(2026, 11, 11, 5, 59, 0, 0, time.UTC)
	user := func(roles ...string) *Principal {
		return &Principal{Type: PrincipalUser, Subject: "alice", Roles: roles}
	}
	svc := &Principal{Type: PrincipalServiceAccount, Subject: "svc-ci", Roles: []string{"developer"}}
	scoped := &Principal{Type: PrincipalServiceAccount, Subject: "svc-ro", Roles: []string{"admin"}, Scopes: []string{"repo:read", "tools:read_*"}}
	teamA := &Principal{Type: PrincipalUser, Subject: "bob", Attributes: map[string]string{"team": "a"}}

	cases := []struct {
		name  string
		req   Request
		allow bool
		rule  string
		why   string
	}{
		{"viewer reads repo", Request{Principal: user("viewer"), Permission: "repo:read"}, true, "role:viewer", ""},
		{"viewer cannot write", Request{Principal: user("viewer"), Permission: "repo:write", Workspace: "/srv/app"}, false, "", "no role or rule"},
		{"workspace deny fails closed", Request{Principal: user("developer"), Permission: "repo:write"}, false, "protect-secrets", "workspace missing"},
		{"viewer wildcard tool", Request{Principal: user("viewer"), Permission: "tools:read_file"}, true, "role:viewer", ""},
		{"developer inherits viewer", Request{Principal: user("developer"), Permission: "repo:read"}, true, "role:viewer", "inherited via"},
		{"developer browser wildcard", Request{Principal: user("developer"), Permission: "tools:browser_navigate"}, true, "role:developer", ""},
		{"developer not other tools", Request{Principal: user("developer"), Permission: "tools:mcp_call"}, false, "", ""},
		{"admin star", Request{Principal: user("admin"), Permission: "billing:delete"}, true, "role:admin", ""},
		{"default role", Request{Principal: user(), Permission: "session:read"}, true, "role:authenticated", "default role"},
		{"unknown role grants nothing", Request{Principal: user("ghost"), Permission: "repo:read"}, false, "", ""},
		{"deny overrides admin in prod", Request{Principal: user("admin"), Permission: "tools:shell", Environment: "prod"}, false, "no-shell-in-prod", ""},
		{"shell fine in dev", Request{Principal: user("developer"), Permission: "tools:shell", Environment: "dev"}, true, "role:developer", ""},
		{"deny applies when env unknown (fail closed)", Request{Principal: user("developer"), Permission: "tools:shell"}, false, "no-shell-in-prod", "environment missing"},
		{"env match case-insensitive", Request{Principal: user("developer"), Permission: "tools:shell", Environment: "PROD"}, false, "no-shell-in-prod", ""},
		{"secrets workspace denied", Request{Principal: user("admin"), Permission: "repo:write", Workspace: "/srv/secrets/vault", Environment: "dev"}, false, "protect-secrets", ""},
		{"secrets path traversal cleaned", Request{Principal: user("admin"), Permission: "repo:write", Workspace: "/srv/app/../secrets/x", Environment: "dev"}, false, "protect-secrets", ""},
		{"other workspace ok", Request{Principal: user("developer"), Permission: "repo:write", Workspace: "/srv/app", Environment: "dev"}, true, "role:developer", ""},
		{"bots no browser", Request{Principal: svc, Permission: "tools:browser_click"}, false, "bots-no-browser", ""},
		{"deploy on weekday", Request{Principal: user("developer"), Permission: "deploy:run", Time: wed}, true, "business-hours-deploy", ""},
		{"no deploy on weekend", Request{Principal: user("developer"), Permission: "deploy:run", Time: sat}, false, "", "outside allowed days"},
		{"no deploy at night", Request{Principal: user("developer"), Permission: "deploy:run", Time: night}, false, "", "outside time window"},
		{"viewer cannot deploy", Request{Principal: user("viewer"), Permission: "deploy:run", Time: wed}, false, "", ""},
		{"night batch wraps midnight", Request{Principal: svc, Permission: "batch:run", Time: night}, true, "night-batch", ""},
		{"night batch early morning", Request{Principal: svc, Permission: "batch:run", Time: early}, true, "night-batch", ""},
		{"night batch not midday", Request{Principal: svc, Permission: "batch:run", Time: wed}, false, "", ""},
		{"batch principal glob", Request{Principal: user("admin"), Permission: "batch:run", Time: night}, true, "role:admin", ""},
		{"scoped key within scope", Request{Principal: scoped, Permission: "repo:read"}, true, "role:admin", ""},
		{"scoped key outside scope", Request{Principal: scoped, Permission: "repo:write", Environment: "dev", Workspace: "/srv/app"}, false, "scope", "outside the credential's scopes"},
		{"attribute from principal", Request{Principal: teamA, Permission: "reports:read"}, true, "team-a-only", ""},
		{"request attribute overrides", Request{Principal: teamA, Permission: "reports:read", Attributes: map[string]string{"team": "b"}}, false, "", "attribute team not matched"},
		{"allow needs attribute present", Request{Principal: user("viewer"), Permission: "reports:read"}, false, "", "missing"},
		{"nil principal", Request{Permission: "repo:read"}, false, "", "unauthenticated"},
		{"wildcard request rejected", Request{Principal: user("admin"), Permission: "repo:*"}, false, "", "invalid permission"},
		{"malformed request", Request{Principal: user("admin"), Permission: "repo"}, false, "", "invalid permission"},
		{"uppercase request", Request{Principal: user("admin"), Permission: "Repo:Read"}, false, "", "invalid permission"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.req.Time.IsZero() {
				tc.req.Time = wed
			}
			d := e.Authorize(tc.req)
			if d.Allowed != tc.allow {
				t.Fatalf("allowed = %v, want %v (%s)", d.Allowed, tc.allow, d.Reason)
			}
			if tc.rule != "" && d.MatchedRule != tc.rule {
				t.Fatalf("matched = %q, want %q (%s)", d.MatchedRule, tc.rule, d.Reason)
			}
			if tc.why != "" && !strings.Contains(d.Reason, tc.why) {
				t.Fatalf("reason %q lacks %q", d.Reason, tc.why)
			}
			if d.Reason == "" {
				t.Fatal("decision without reason")
			}
		})
	}
}

func TestRolesFromClaims(t *testing.T) {
	e := testEngine(t)
	claims := map[string]any{
		"groups":                    []any{"sales", "platform"},
		"realm_access":              map[string]any{"roles": []any{"gta-admin"}},
		"https://example.com/roles": "audit",
		"resource_access":           map[string]any{"gta": map[string]any{"roles": []any{"viewer"}}},
	}
	got := strings.Join(e.RolesFromClaims(claims), ",")
	if got != "admin,auditor,developer,viewer" {
		t.Fatalf("roles = %s", got)
	}
	if r := e.RolesFromClaims(map[string]any{"groups": "eng"}); len(r) != 1 || r[0] != "developer" {
		t.Fatalf("string claim: %v", r)
	}
	if r := e.RolesFromClaims(map[string]any{"groups": []any{1, true}}); len(r) != 0 {
		t.Fatalf("non-string claims mapped: %v", r)
	}
	eff := e.EffectiveRoles(&Principal{Subject: "x", Roles: []string{"admin"}})
	if strings.Join(eff, ",") != "admin,authenticated,developer,viewer" {
		t.Fatalf("effective = %v", eff)
	}
}

func TestPolicyValidation(t *testing.T) {
	bad := map[string]string{
		"cycle":              `{"version":1,"roles":{"a":{"inherits":["b"]},"b":{"inherits":["c"]},"c":{"inherits":["a"]}}}`,
		"self cycle":         `{"version":1,"roles":{"a":{"inherits":["a"]}}}`,
		"unknown parent":     `{"version":1,"roles":{"a":{"inherits":["zz"]}}}`,
		"unknown field":      `{"version":1,"roles":{"a":{"permision":["x:y"]}}}`,
		"top unknown field":  `{"version":1,"roles":{"a":{}},"rule":[]}`,
		"trailing":           `{"version":1,"roles":{"a":{}}} {}`,
		"version":            `{"version":2,"roles":{"a":{}}}`,
		"no roles":           `{"version":1,"roles":{}}`,
		"bad perm":           `{"version":1,"roles":{"a":{"permissions":["repo"]}}}`,
		"bad perm chars":     `{"version":1,"roles":{"a":{"permissions":["Repo:Write"]}}}`,
		"two colons":         `{"version":1,"roles":{"a":{"permissions":["a:b:c"]}}}`,
		"bad role name":      `{"version":1,"roles":{"a b":{}}}`,
		"default unknown":    `{"version":1,"default_roles":["x"],"roles":{"a":{}}}`,
		"mapping role":       `{"version":1,"roles":{"a":{}},"group_mappings":[{"claim":"groups","values":["g"],"roles":["x"]}]}`,
		"mapping no values":  `{"version":1,"roles":{"a":{}},"group_mappings":[{"claim":"groups","values":[],"roles":["a"]}]}`,
		"mapping no claim":   `{"version":1,"roles":{"a":{}},"group_mappings":[{"values":["g"],"roles":["a"]}]}`,
		"rule no id":         `{"version":1,"roles":{"a":{}},"rules":[{"effect":"allow","permissions":["a:b"]}]}`,
		"rule dup id":        `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"]},{"id":"r","effect":"deny","permissions":["a:b"]}]}`,
		"rule effect":        `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"maybe","permissions":["a:b"]}]}`,
		"rule no perms":      `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow"}]}`,
		"rule unknown role":  `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"subjects":{"roles":["x"]}}]}`,
		"rule bad type":      `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"subjects":{"types":["robot"]}}]}`,
		"bad day":            `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"conditions":{"time":{"days":["funday"]}}}]}`,
		"bad clock":          `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"conditions":{"time":{"start":"25:00","end":"01:00"}}}]}`,
		"half window":        `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"conditions":{"time":{"start":"09:00"}}}]}`,
		"equal window":       `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"conditions":{"time":{"start":"09:00","end":"09:00"}}}]}`,
		"bad tz":             `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"conditions":{"time":{"days":["mon"],"timezone":"Mars/Olympus"}}}]}`,
		"empty time":         `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"conditions":{"time":{}}}]}`,
		"bad glob":           `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"conditions":{"workspaces":["/srv/[x"]}}]}`,
		"empty conditions":   `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"conditions":{}}]}`,
		"empty attr values":  `{"version":1,"roles":{"a":{}},"rules":[{"id":"r","effect":"allow","permissions":["a:b"],"conditions":{"attributes":{"team":[]}}}]}`,
		"wrong type":         `{"version":"1","roles":{"a":{}}}`,
		"duplicate inherits": `{"version":1,"roles":{"a":{},"b":{"inherits":["a","a"]}}}`,
	}
	for name, src := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePolicy([]byte(src)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	_, err := ParsePolicy([]byte(bad["cycle"]))
	if err == nil || !strings.Contains(err.Error(), "a -> b -> c -> a") {
		t.Fatalf("cycle path not reported: %v", err)
	}
	// Diamond inheritance is not a cycle.
	if _, err := ParsePolicy([]byte(`{"version":1,"roles":{"base":{},"l":{"inherits":["base"]},"r":{"inherits":["base"]},"top":{"inherits":["l","r"]}}}`)); err != nil {
		t.Fatalf("diamond rejected: %v", err)
	}
}

func TestReloadableEngine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authz.json")
	if err := os.WriteFile(path, []byte(testPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := LoadPolicyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := NewReloadableEngine(e)
	p := &Principal{Subject: "a", Roles: []string{"viewer"}}
	if !r.Authorize(Request{Principal: p, Permission: "repo:read"}).Allowed {
		t.Fatal("initial policy")
	}
	_ = os.WriteFile(path, []byte(`{"version":1,"roles":{"viewer":{"permissions":[]}}}`), 0o600)
	if err := r.ReloadFile(path); err != nil {
		t.Fatal(err)
	}
	if r.Authorize(Request{Principal: p, Permission: "repo:read"}).Allowed {
		t.Fatal("reload not applied")
	}
	_ = os.WriteFile(path, []byte(`{"version":1,`), 0o600)
	if err := r.ReloadFile(path); err == nil {
		t.Fatal("invalid policy loaded")
	}
	if r.Load() == nil {
		t.Fatal("engine lost after failed reload")
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		p, s string
		want bool
	}{
		{"*", "", true},
		{"*", "anything", true},
		{"browser_*", "browser_navigate", true},
		{"browser_*", "browser_", true},
		{"browser_*", "browse", false},
		{"*_file", "read_file", true},
		{"r?ad", "read", true},
		{"r?ad", "rad", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"", "", true},
		{"", "x", false},
		{"a**", "a", true},
		{strings.Repeat("*a", 30) + "b", strings.Repeat("a", 60), false},
	}
	for _, c := range cases {
		if got := globMatch(c.p, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v", c.p, c.s, got)
		}
	}
	if !MatchPermission("tools:browser_*", "tools:browser_click") || MatchPermission("tools:browser_*", "tools:shell") || MatchPermission("bad", "tools:x") {
		t.Fatal("MatchPermission")
	}
}

func BenchmarkAuthorize(b *testing.B) {
	e := testEngine(b)
	p := &Principal{Type: PrincipalUser, Subject: "alice", Roles: []string{"developer"}}
	req := Request{Principal: p, Permission: "tools:browser_click", Environment: "dev", Workspace: "/srv/app", Time: time.Now()}
	b.ReportAllocs()
	for b.Loop() {
		if !e.Authorize(req).Allowed {
			b.Fatal("denied")
		}
	}
}
