package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/runestack/rune/internal/config"
	"github.com/runestack/rune/pkg/log"
	"github.com/runestack/rune/pkg/store"
)

// mustOpenStore runs startup phases 3-4 (RUNE-313): the state store, the
// dev-mode storage overlay, and the registry-secret bootstrap.
//
// Ordering constraint: the registry bootstrap writes docker.registries into
// the GLOBAL viper, which runnerManager.Initialize() reads from
// apiServer.Start(). It must therefore complete before the control-plane
// phase, not merely "before runner construction".
func mustOpenStore(b *boot) *controlPlane {
	logger := b.logger
	resolvedRunefile := b.runefile
	closers := b.closers

	// Open state store via helper
	stateStore, appCfg, _, err := openStateStore(logger, resolvedRunefile, *dataDir)
	if err != nil {
		logger.Error("Failed to open state store", log.Err(err))
		os.Exit(1)
	}
	closers.push("state-store", func() { stateStore.Close() })

	// Dev-mode storage overlay: when --dev-mode is on, force the
	// local/local-host drivers into a laptop-friendly layout —
	// allowCreateMissing=true and a default ~/.rune/volumes root
	// that's mkdir'able under the developer's home (the production
	// /var/lib/rune/volumes default usually requires root). Operator
	// config wins; we never overwrite explicit values.
	if *devMode && appCfg != nil {
		applyDevModeStorageOverlay(&appCfg.Storage, logger)
	}

	// Bootstrap and resolve registry secrets into viper before runner init
	if err := bootstrapAndResolveRegistryAuth(appCfg, stateStore, logger); err != nil {
		logger.Error("Failed to bootstrap/resolve registry auth", log.Err(err))
		os.Exit(1)
	}

	return &controlPlane{store: stateStore, cfg: appCfg}
}

func openStateStore(logger log.Logger, cfgFile, dataDirPath string) (store.Store, *config.Config, string, error) {
	storeDir := filepath.Join(dataDirPath, "store")
	if err := os.MkdirAll(storeDir, 0755); err != nil {
		return nil, nil, storeDir, fmt.Errorf("create data dir: %w", err)
	}
	appCfg, _ := config.Load(cfgFile)
	if appCfg.Secret.Encryption.KEK.Source == "file" && appCfg.Secret.Encryption.KEK.File == "" {
		appCfg.Secret.Encryption.KEK.File = filepath.Join(dataDirPath, "kek.b64")
	}
	st := store.NewBadgerStoreWithOptions(logger, store.StoreOptions{
		Path:                    storeDir,
		SecretEncryptionEnabled: appCfg.Secret.Encryption.Enabled,
		KEKOptions:              appCfg.KEKOptions(),
		SecretLimits:            appCfg.Secret.Limits,
		ConfigLimits:            appCfg.ConfigResource.Limits,
	})
	if err := st.Open(storeDir); err != nil {
		return nil, nil, storeDir, err
	}
	return st, appCfg, storeDir, nil
}

// applyDevModeStorageOverlay mutates the operator-provided Storage
// config so the local storage drivers run cleanly on a developer
// laptop under --dev-mode:
//
//   - allowCreateMissing is forced to true on the local-host driver
//     (so MyService.volumes hostPath: ~/proj/data auto-mkdirs instead
//     of failing with ErrInvalidConfig).
//   - ~/.rune/volumes is appended to local-host.hostPathAllowlist if
//     not already present, giving services a default writable area.
//   - local.localVolumeRoot defaults to ~/.rune/volumes when unset
//     (production default is /var/lib/rune/volumes which usually
//     requires root on Linux and never exists on macOS).
//
// Operator-provided values always win — every key is only filled in
// when the operator left it unset. Callers should only invoke this
// when *devMode is true.
func applyDevModeStorageOverlay(s *config.Storage, logger log.Logger) {
	if s == nil {
		return
	}
	if s.Drivers == nil {
		s.Drivers = make(map[string]map[string]any, 2)
	}

	home, _ := os.UserHomeDir()
	devVolRoot := ""
	if home != "" {
		devVolRoot = filepath.Join(home, ".rune", "volumes")
	}

	// local-host driver overlay.
	hostCfg := s.Drivers["local-host"]
	if hostCfg == nil {
		hostCfg = make(map[string]any, 2)
	}
	if _, set := hostCfg["allowCreateMissing"]; !set {
		hostCfg["allowCreateMissing"] = true
	}
	if devVolRoot != "" {
		raw, ok := hostCfg["hostPathAllowlist"]
		var allow []any
		if ok {
			if existing, ok2 := raw.([]any); ok2 {
				allow = existing
			} else if existing, ok2 := raw.([]string); ok2 {
				allow = make([]any, 0, len(existing))
				for _, v := range existing {
					allow = append(allow, v)
				}
			}
		}
		present := false
		for _, v := range allow {
			if s, ok := v.(string); ok && s == devVolRoot {
				present = true
				break
			}
		}
		if !present {
			allow = append(allow, devVolRoot)
		}
		hostCfg["hostPathAllowlist"] = allow
	}
	s.Drivers["local-host"] = hostCfg

	// local (managed) driver overlay.
	if devVolRoot != "" {
		mgr := s.Drivers["local"]
		if mgr == nil {
			mgr = make(map[string]any, 1)
		}
		if _, set := mgr["localVolumeRoot"]; !set {
			mgr["localVolumeRoot"] = devVolRoot
		}
		s.Drivers["local"] = mgr
	}

	logger.Info("Dev-mode storage overlay applied",
		log.Str("dev_volume_root", devVolRoot),
		log.Bool("allow_create_missing", true))
}
