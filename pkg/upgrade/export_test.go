package upgrade

import "github.com/runestack/rune/pkg/systemd"

func defaultUnitOptionsForTest() systemd.UnitOptions {
	return systemd.DefaultUnitOptions()
}
