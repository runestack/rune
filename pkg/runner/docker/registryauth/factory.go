package registryauth

import (
	"context"

	"github.com/runestack/rune/pkg/log"
)

// BuildProviders constructs providers from normalized registries configuration.
// Each entry is expected to contain keys: name, registry, auth{type, username, password, token, region, dockerconfigjson}
//
// Entries with no auth block, missing type, or an unknown type are skipped
// with a warn log so misconfigured registries don't silently disappear into
// anonymous-pull territory (see RUNE-? — the GHCR symptom that surfaced this).
func BuildProviders(ctx context.Context, regs []map[string]any) []Provider {
	logger := log.GetDefaultLogger().WithComponent("registryauth")
	var out []Provider
	for _, r := range regs {
		host, _ := r["registry"].(string)
		name, _ := r["name"].(string)
		auth, _ := r["auth"].(map[string]any)
		if auth == nil {
			logger.Warn("Registry has no auth block; image pulls will be anonymous",
				log.Str("name", name), log.Str("registry", host))
			continue
		}
		t, _ := auth["type"].(string)
		switch t {
		case "basic":
			out = append(out, NewBasicTokenProvider(BasicTokenConfig{
				Registry: host,
				Username: str(auth["username"]),
				Password: str(auth["password"]),
			}))
		case "token":
			out = append(out, NewBasicTokenProvider(BasicTokenConfig{
				Registry: host,
				Token:    str(auth["token"]),
			}))
		case "dockerconfigjson":
			if raw := str(auth["dockerconfigjson"]); raw != "" {
				out = append(out, NewDockerConfigJSONProvider(host, raw))
			} else {
				logger.Warn("Registry type=dockerconfigjson but dockerconfigjson value is empty",
					log.Str("name", name), log.Str("registry", host))
			}
		case "ecr":
			out = append(out, NewECRProvider(ECRConfig{Registry: host, Region: str(auth["region"])}))
		case "":
			logger.Warn("Registry auth has no type; image pulls will be anonymous (did fromSecret resolve?)",
				log.Str("name", name), log.Str("registry", host))
		default:
			logger.Warn("Registry has unknown auth type; image pulls will be anonymous",
				log.Str("name", name), log.Str("registry", host), log.Str("type", t))
		}
	}
	return out
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
