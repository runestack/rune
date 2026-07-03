package registryauth

import (
	"context"

	"cloud.google.com/go/compute/metadata"
	"github.com/runestack/rune/pkg/log"
)

// AmbientProviders returns providers derived from the node environment
// rather than explicit [[docker.registries]] config. They are appended
// after all configured providers, so explicit config always wins:
//
//   - on GCE, the instance service account for *.pkg.dev / gcr.io
//     hosts (what enable_artifact_registry_access in the Terraform
//     module implies — issue #144);
//   - the docker CLI config of the user runed runs as, so a plain
//     `docker login` on the node works for any registry.
//
// The GCP provider precedes the docker-config one deliberately: for
// Google hosts a metadata token is always fresh, while an on-disk
// `docker login` entry made with an SA token rots within the hour.
func AmbientProviders(ctx context.Context) []Provider {
	var out []Provider
	if metadata.OnGCE() {
		log.GetDefaultLogger().WithComponent("registryauth").Debug(
			"On GCE; enabling metadata service-account auth for Artifact Registry / GCR pulls")
		out = append(out, NewGCPProvider(GCPConfig{}))
	}
	out = append(out, NewDockerConfigFileProvider())
	return out
}
