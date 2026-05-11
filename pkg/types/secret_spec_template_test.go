package types

import (
	"testing"
)

func TestSecretSpec_RestoreTemplateReferences_PreservesTemplatesInData(t *testing.T) {
	t.Parallel()

	yamlContent := []byte(`
secret:
  name: db-credentials
  type: static
  data:
    DATABASE_URL: "postgres://u:{{ secret:db-password/value }}@{{ secret:db-host/value }}:5432/api"
    plain: hello
`)
	cf, err := ParseCastFileFromBytes(yamlContent, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	secrets, err := cf.GetSecrets()
	if err != nil {
		t.Fatalf("GetSecrets: %v", err)
	}
	if len(secrets) != 1 {
		t.Fatalf("want 1 secret, got %d", len(secrets))
	}
	s := secrets[0]

	wantURL := "postgres://u:{{ secret:db-password/value }}@{{ secret:db-host/value }}:5432/api"
	if s.Data["DATABASE_URL"] != wantURL {
		t.Fatalf("template not restored:\n got %q\nwant %q", s.Data["DATABASE_URL"], wantURL)
	}
	if s.Data["plain"] != "hello" {
		t.Fatalf("plain value altered: %q", s.Data["plain"])
	}
}

func TestSecretSpec_RestoreTemplateReferences_SecretsSequenceForm(t *testing.T) {
	t.Parallel()

	yamlContent := []byte(`
secrets:
  - name: db-host
    type: static
    data:
      value: postgres.internal
  - name: db-credentials
    type: static
    data:
      URL: "{{ secret:db-host/value }}"
`)
	cf, err := ParseCastFileFromBytes(yamlContent, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	secrets, err := cf.GetSecrets()
	if err != nil {
		t.Fatalf("GetSecrets: %v", err)
	}
	if len(secrets) != 2 {
		t.Fatalf("want 2 secrets, got %d", len(secrets))
	}
	var creds *Secret
	for _, s := range secrets {
		if s.Name == "db-credentials" {
			creds = s
		}
	}
	if creds == nil {
		t.Fatalf("db-credentials not found")
	}
	want := "{{ secret:db-host/value }}"
	if creds.Data["URL"] != want {
		t.Fatalf("template not restored:\n got %q\nwant %q", creds.Data["URL"], want)
	}
}
