package build

import (
	"testing"
	"time"

	cacheimporttypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/client"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestMergeCacheConfigs(t *testing.T) {
	now := time.Now().UTC()
	// node A: base layer -> child layer, record 1 depends on record 0
	cfgA := cacheimporttypes.CacheConfig{
		Layers: []cacheimporttypes.CacheLayer{
			{Blob: digest.FromString("a0"), ParentIndex: -1},
			{Blob: digest.FromString("a1"), ParentIndex: 0},
		},
		Records: []cacheimporttypes.CacheRecord{
			{Digest: digest.FromString("ra0"), Results: []cacheimporttypes.CacheResult{{LayerIndex: 0, CreatedAt: now}}},
			{
				Digest:  digest.FromString("ra1"),
				Results: []cacheimporttypes.CacheResult{{LayerIndex: 1, CreatedAt: now}},
				Inputs:  [][]cacheimporttypes.CacheInput{{{Selector: "/", LinkIndex: 0}}},
			},
		},
	}
	// node B: same shape plus chained results
	cfgB := cacheimporttypes.CacheConfig{
		Layers: []cacheimporttypes.CacheLayer{
			{Blob: digest.FromString("b0"), ParentIndex: -1},
			{Blob: digest.FromString("b1"), ParentIndex: 0},
		},
		Records: []cacheimporttypes.CacheRecord{
			{Digest: digest.FromString("rb0")},
			{
				Digest:         digest.FromString("rb1"),
				ChainedResults: []cacheimporttypes.ChainedResult{{LayerIndexes: []int{0, 1}, CreatedAt: now}},
				Inputs:         [][]cacheimporttypes.CacheInput{{{LinkIndex: 0}}},
			},
		},
	}

	out := mergeCacheConfigs([]cacheimporttypes.CacheConfig{cfgA, cfgB})

	require.Len(t, out.Layers, 4)
	require.Equal(t, -1, out.Layers[0].ParentIndex)
	require.Equal(t, 0, out.Layers[1].ParentIndex)
	require.Equal(t, -1, out.Layers[2].ParentIndex)
	require.Equal(t, 2, out.Layers[3].ParentIndex)
	require.Equal(t, digest.FromString("b1"), out.Layers[3].Blob)

	require.Len(t, out.Records, 4)
	require.Equal(t, 0, out.Records[0].Results[0].LayerIndex)
	require.Equal(t, 1, out.Records[1].Results[0].LayerIndex)
	require.Equal(t, 0, out.Records[1].Inputs[0][0].LinkIndex)
	require.Equal(t, "/", out.Records[1].Inputs[0][0].Selector)
	require.Equal(t, []int{2, 3}, out.Records[3].ChainedResults[0].LayerIndexes)
	require.Equal(t, 2, out.Records[3].Inputs[0][0].LinkIndex)

	// inputs must not be modified
	require.Equal(t, 0, cfgB.Records[1].Inputs[0][0].LinkIndex)
	require.Equal(t, []int{0, 1}, cfgB.Records[1].ChainedResults[0].LayerIndexes)
}

func TestRegistryCacheExportForMerge(t *testing.T) {
	reg := client.CacheOptionsEntry{Type: "registry", Attrs: map[string]string{"ref": "example.com/foo:cache"}}

	e, ok := registryCacheExportForMerge([]client.CacheOptionsEntry{reg})
	require.True(t, ok)
	require.Equal(t, "example.com/foo:cache", e.Attrs["ref"])

	_, ok = registryCacheExportForMerge([]client.CacheOptionsEntry{reg, {Type: "inline"}})
	require.True(t, ok)

	_, ok = registryCacheExportForMerge([]client.CacheOptionsEntry{reg, {Type: "local", Attrs: map[string]string{"dest": "/tmp"}}})
	require.False(t, ok)

	_, ok = registryCacheExportForMerge([]client.CacheOptionsEntry{{Type: "gha"}})
	require.False(t, ok)

	_, ok = registryCacheExportForMerge(nil)
	require.False(t, ok)
}

func TestDedupeDescriptors(t *testing.T) {
	a := ocispecs.Descriptor{Digest: digest.FromString("a")}
	b := ocispecs.Descriptor{Digest: digest.FromString("b")}
	require.Equal(t, []ocispecs.Descriptor{a, b}, dedupeDescriptors([]ocispecs.Descriptor{a, b, a}))
}
