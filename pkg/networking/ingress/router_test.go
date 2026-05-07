package ingress

import "testing"

func TestRouter_MatchByHost(t *testing.T) {
	r := NewRouter()
	r.Apply([]Route{
		{Host: "Api.Example.com", Namespace: "prod", Service: "api", Port: 8080},
		{Host: "web.example.com", Namespace: "prod", Service: "web", Port: 80},
	})
	rt, ok := r.Match("api.example.com:443", "/v1/users")
	if !ok || rt.Service != "api" || rt.Port != 8080 {
		t.Fatalf("api lookup: got %+v ok=%v", rt, ok)
	}
	rt, ok = r.Match("web.example.com", "/")
	if !ok || rt.Service != "web" {
		t.Fatalf("web lookup: got %+v ok=%v", rt, ok)
	}
	if _, ok := r.Match("nope.example.com", "/"); ok {
		t.Fatalf("nope should not match")
	}
}

func TestRouter_PathPrefix(t *testing.T) {
	r := NewRouter()
	r.Apply([]Route{
		{Host: "h", Namespace: "n", Service: "longer", Port: 1, Path: "/api/v2"},
		{Host: "h", Namespace: "n", Service: "shorter", Port: 2, Path: "/api"},
		{Host: "h", Namespace: "n", Service: "default", Port: 3},
	})
	got, _ := r.Match("h", "/api/v2/users")
	if got.Service != "longer" {
		t.Fatalf("expected longer prefix, got %q", got.Service)
	}
	got, _ = r.Match("h", "/api/v1/users")
	if got.Service != "shorter" {
		t.Fatalf("expected shorter prefix, got %q", got.Service)
	}
	got, _ = r.Match("h", "/other")
	if got.Service != "default" {
		t.Fatalf("expected default, got %q", got.Service)
	}
}

func TestRouter_ApplyReplacesAtomically(t *testing.T) {
	r := NewRouter()
	r.Apply([]Route{{Host: "h1", Namespace: "n", Service: "s", Port: 1}})
	r.Apply([]Route{{Host: "h2", Namespace: "n", Service: "s", Port: 1}})
	if _, ok := r.Match("h1", "/"); ok {
		t.Fatalf("h1 should be gone")
	}
	if _, ok := r.Match("h2", "/"); !ok {
		t.Fatalf("h2 should match")
	}
	hs := r.Hosts()
	if len(hs) != 1 || hs[0] != "h2" {
		t.Fatalf("hosts: %v", hs)
	}
}

func TestRouter_DropsInvalid(t *testing.T) {
	r := NewRouter()
	r.Apply([]Route{
		{Host: "", Service: "s", Port: 1},
		{Host: "h", Service: "", Port: 1},
		{Host: "h", Service: "s", Port: 0},
		{Host: "h", Service: "s", Port: 1},
	})
	hs := r.Hosts()
	if len(hs) != 1 {
		t.Fatalf("expected 1 host, got %v", hs)
	}
}
