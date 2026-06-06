package docker

import "testing"

func TestDockerConfig_LogConfig(t *testing.T) {
	// Defaults impose json-file rotation so host logs don't grow unbounded.
	def := DefaultDockerConfig().logConfig()
	if def.Type != "json-file" {
		t.Fatalf("default Type: want json-file, got %q", def.Type)
	}
	if def.Config["max-size"] != "10m" || def.Config["max-file"] != "3" {
		t.Errorf("default rotation: want 10m/3, got %+v", def.Config)
	}

	// Empty LogMaxSize disables rotation (inherit the daemon default).
	if dis := (&DockerConfig{LogMaxSize: ""}).logConfig(); dis.Type != "" || dis.Config != nil {
		t.Errorf("empty LogMaxSize should disable rotation, got %+v", dis)
	}

	// max-file <= 0 is omitted.
	lc := (&DockerConfig{LogMaxSize: "5m", LogMaxFile: 0}).logConfig()
	if lc.Config["max-size"] != "5m" {
		t.Errorf("max-size: want 5m, got %+v", lc.Config)
	}
	if _, ok := lc.Config["max-file"]; ok {
		t.Errorf("max-file should be omitted when <= 0, got %+v", lc.Config)
	}
}
