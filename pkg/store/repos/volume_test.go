package repos

import (
	"context"
	"testing"

	"github.com/runestack/rune/pkg/store"
	"github.com/runestack/rune/pkg/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A volume's ID becomes a host mount path segment, so a client-supplied
// ID must be path-safe and may not change afterwards.
func TestVolumeRepo_IDValidatedAndImmutable(t *testing.T) {
	ctx := context.Background()
	r := NewVolumeRepo(store.NewTestStore())

	for _, id := range []string{"../prod/data-api-0", "a/b", ".."} {
		err := r.Create(ctx, &types.Volume{ID: id, Name: "data", Namespace: "trial", StorageClassName: "fast"})
		assert.Error(t, err, id)
	}

	v := &types.Volume{Name: "data", Namespace: "trial", StorageClassName: "fast"}
	require.NoError(t, r.Create(ctx, v))
	id := v.ID

	upd := *v
	upd.ID = "../prod/data-api-0"
	assert.Error(t, r.Update(ctx, &upd))

	upd.ID = ""
	require.NoError(t, r.Update(ctx, &upd))
	assert.Equal(t, id, upd.ID)
}
