package events

import "testing"

func TestAuditPropsKeepOnlyTheEmailDomain(t *testing.T) {
	got := auditProps("reg1", "mod9", map[string]any{
		"email":  "someone@hmu.gr",
		"banned": true,
		"step":   "code",
	})
	if _, leaked := got["email"]; leaked {
		t.Fatalf("email address reached the properties: %v", got)
	}
	if got["email_domain"] != "hmu.gr" || got["banned"] != true || got["step"] != "code" {
		t.Fatalf("properties = %v", got)
	}
	if got["registration_id"] != "reg1" || got["actor_id"] != "mod9" {
		t.Fatalf("ids missing: %v", got)
	}
}

func TestAuditPropsNilDetail(t *testing.T) {
	if got := auditProps("", "", nil); len(got) != 0 {
		t.Fatalf("properties = %v", got)
	}
}
