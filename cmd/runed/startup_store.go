package main

import (
	"os"

	"github.com/runestack/rune/pkg/log"
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
