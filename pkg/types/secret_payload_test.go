package types

import "testing"

// Secret.Data is json:"-" so ordinary marshaling drops it — every secret cast
// through the release payload path arrived at the server EMPTY. The payload
// codec must round-trip Data.
func TestSecretPayload_RoundTripsData(t *testing.T) {
	in := &Secret{
		Name:      "creds",
		Namespace: "observability",
		Type:      "static",
		Data:      map[string]string{"username": "runesight", "password": "hunter2"},
	}
	b, err := MarshalSecretPayload(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := UnmarshalSecretPayload(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Name != "creds" || out.Namespace != "observability" {
		t.Errorf("identity lost: %+v", out)
	}
	if out.Data["username"] != "runesight" || out.Data["password"] != "hunter2" {
		t.Errorf("Data lost in round-trip (the bug): %+v", out.Data)
	}
}

func TestSecretPayload_NilDataStaysNil(t *testing.T) {
	b, err := MarshalSecretPayload(&Secret{Name: "empty", Namespace: "default"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := UnmarshalSecretPayload(b)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Data) != 0 {
		t.Errorf("want no data, got %+v", out.Data)
	}
}
