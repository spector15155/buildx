package build

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/distribution/reference"
	"github.com/docker/buildx/util/imagetools"
	"github.com/docker/buildx/util/progress"
	"github.com/docker/buildx/util/resolver"
	cacheimporttypes "github.com/moby/buildkit/cache/remotecache/v1/types"
	"github.com/moby/buildkit/client"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// exporterResponseCacheManifestKey is the key BuildKit uses to return the
// descriptor of the cache manifest written by a cache exporter.
const exporterResponseCacheManifestKey = "cache.manifest"

// registryCacheExportForMerge returns the registry cache export that needs its
// per-node manifests merged after a multi-node build. BuildKit only reports a
// single cache manifest descriptor per solve, so merging is only possible when
// the registry exporter is the only non-inline cache export.
func registryCacheExportForMerge(exports []client.CacheOptionsEntry) (*client.CacheOptionsEntry, bool) {
	var found *client.CacheOptionsEntry
	var others int
	for i, e := range exports {
		switch e.Type {
		case "inline":
		case "registry":
			if found != nil {
				others++
				continue
			}
			found = &exports[i]
		default:
			others++
		}
	}
	if found == nil {
		return nil, false
	}
	if others > 0 {
		logrus.Warnf("multi-node build with multiple cache exports: registry cache %s will only contain the cache of the last node to finish", found.Attrs["ref"])
		return nil, false
	}
	return found, true
}

// mergeMultiNodeRegistryCache combines the registry cache manifests written by
// each node of a multi-node build into a single cache manifest. Without this,
// every node pushes its own manifest to the same ref and only the last one
// survives.
func mergeMultiNodeRegistryCache(ctx context.Context, pw progress.Writer, exports []client.CacheOptionsEntry, imageopt imagetools.Opt, res []*client.SolveResponse) error {
	e, ok := registryCacheExportForMerge(exports)
	if !ok {
		return nil
	}
	ref := e.Attrs["ref"]
	if ref == "" {
		return nil
	}

	var descs []ocispecs.Descriptor
	for _, r := range res {
		if r == nil {
			continue
		}
		s, ok := r.ExporterResponse[exporterResponseCacheManifestKey]
		if !ok || s == "" {
			continue
		}
		var desc ocispecs.Descriptor
		if err := json.Unmarshal([]byte(s), &desc); err != nil {
			return errors.Wrapf(err, "failed to unmarshal cache manifest descriptor %s", s)
		}
		descs = append(descs, desc)
	}
	if len(descs) < 2 {
		return nil
	}

	err := progress.Write(pw, fmt.Sprintf("merging cache manifest %s", ref), func() error {
		if ok, _ := strconv.ParseBool(e.Attrs["registry.insecure"]); ok {
			nn, err := reference.ParseNormalizedNamed(ref)
			if err != nil {
				return err
			}
			insecureTrue := true
			httpTrue := true
			imageopt.RegistryConfig = map[string]resolver.RegistryConfig{
				reference.Domain(nn): {
					Insecure:  &insecureTrue,
					PlainHTTP: &httpTrue,
				},
			}
		}
		return mergeRegistryCache(ctx, imagetools.New(imageopt), ref, descs)
	})
	if err != nil {
		if ok, _ := strconv.ParseBool(e.Attrs["ignore-error"]); ok {
			logrus.WithError(err).Warnf("failed to merge multi-node cache manifest %s", ref)
			return nil
		}
		return err
	}
	return nil
}

func mergeRegistryCache(ctx context.Context, r *imagetools.Resolver, ref string, descs []ocispecs.Descriptor) error {
	loc, err := imagetools.ParseLocation(ref)
	if err != nil {
		return err
	}
	if !loc.IsRegistry() {
		return errors.Errorf("cache ref %s is not a registry reference", ref)
	}

	mediaType := descs[0].MediaType
	configs := make([]cacheimporttypes.CacheConfig, 0, len(descs))
	var layers []ocispecs.Descriptor
	var baseManifest *ocispecs.Manifest
	var baseIndex *ocispecs.Index

	for _, desc := range descs {
		if desc.MediaType != mediaType {
			return errors.Errorf("cannot merge cache manifests with different media types %s and %s", mediaType, desc.MediaType)
		}
		dt, err := r.GetDescriptor(ctx, loc, desc)
		if err != nil {
			return errors.Wrapf(err, "failed to fetch cache manifest %s", desc.Digest)
		}

		var configDesc ocispecs.Descriptor
		switch mediaType {
		case ocispecs.MediaTypeImageIndex, images.MediaTypeDockerSchema2ManifestList:
			var idx ocispecs.Index
			if err := json.Unmarshal(dt, &idx); err != nil {
				return errors.Wrapf(err, "failed to unmarshal cache manifest %s", desc.Digest)
			}
			if baseIndex == nil {
				baseIndex = &idx
			}
			for _, m := range idx.Manifests {
				if m.MediaType == cacheimporttypes.CacheConfigMediaTypeV0 {
					configDesc = m
					continue
				}
				layers = append(layers, m)
			}
		case ocispecs.MediaTypeImageManifest:
			var mfst ocispecs.Manifest
			if err := json.Unmarshal(dt, &mfst); err != nil {
				return errors.Wrapf(err, "failed to unmarshal cache manifest %s", desc.Digest)
			}
			if baseManifest == nil {
				baseManifest = &mfst
			}
			configDesc = mfst.Config
			layers = append(layers, mfst.Layers...)
		default:
			return errors.Errorf("unsupported cache manifest media type %s", mediaType)
		}

		if configDesc.MediaType != cacheimporttypes.CacheConfigMediaTypeV0 {
			return errors.Errorf("cache config not found in cache manifest %s", desc.Digest)
		}
		configDt, err := r.GetDescriptor(ctx, loc, configDesc)
		if err != nil {
			return errors.Wrapf(err, "failed to fetch cache config %s", configDesc.Digest)
		}
		var cfg cacheimporttypes.CacheConfig
		if err := json.Unmarshal(configDt, &cfg); err != nil {
			return errors.Wrapf(err, "failed to unmarshal cache config %s", configDesc.Digest)
		}
		configs = append(configs, cfg)
	}

	configDt, err := json.Marshal(mergeCacheConfigs(configs))
	if err != nil {
		return err
	}
	configDesc := ocispecs.Descriptor{
		MediaType: cacheimporttypes.CacheConfigMediaTypeV0,
		Digest:    digest.FromBytes(configDt),
		Size:      int64(len(configDt)),
	}
	layers = dedupeDescriptors(layers)

	var dt []byte
	if baseIndex != nil {
		idx := *baseIndex
		idx.Manifests = append(layers, configDesc)
		dt, err = json.Marshal(idx)
	} else {
		mfst := *baseManifest
		mfst.Layers = layers
		mfst.Config = configDesc
		dt, err = json.Marshal(mfst)
	}
	if err != nil {
		return err
	}
	desc := ocispecs.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(dt),
		Size:      int64(len(dt)),
	}

	// Layer blobs were already pushed to the same repository by each node, so
	// only the new config blob and the merged manifest need to be uploaded.
	if err := r.Push(ctx, loc, configDesc, configDt); err != nil {
		return errors.Wrap(err, "failed to push merged cache config")
	}
	if err := r.Push(ctx, loc, desc, dt); err != nil {
		return errors.Wrap(err, "failed to push merged cache manifest")
	}
	return nil
}

// mergeCacheConfigs concatenates cache configs, shifting layer and record
// indexes so that every reference stays valid within the merged config.
func mergeCacheConfigs(cfgs []cacheimporttypes.CacheConfig) cacheimporttypes.CacheConfig {
	var out cacheimporttypes.CacheConfig
	for _, cfg := range cfgs {
		layerOffset := len(out.Layers)
		recordOffset := len(out.Records)

		for _, l := range cfg.Layers {
			if l.ParentIndex >= 0 {
				l.ParentIndex += layerOffset
			}
			out.Layers = append(out.Layers, l)
		}

		for _, rec := range cfg.Records {
			nrec := cacheimporttypes.CacheRecord{
				Digest: rec.Digest,
			}
			for _, res := range rec.Results {
				res.LayerIndex += layerOffset
				nrec.Results = append(nrec.Results, res)
			}
			for _, ch := range rec.ChainedResults {
				idxs := make([]int, len(ch.LayerIndexes))
				for i, li := range ch.LayerIndexes {
					idxs[i] = li + layerOffset
				}
				ch.LayerIndexes = idxs
				nrec.ChainedResults = append(nrec.ChainedResults, ch)
			}
			if rec.Inputs != nil {
				nrec.Inputs = make([][]cacheimporttypes.CacheInput, len(rec.Inputs))
				for i, inputs := range rec.Inputs {
					ninputs := make([]cacheimporttypes.CacheInput, len(inputs))
					for j, inp := range inputs {
						inp.LinkIndex += recordOffset
						ninputs[j] = inp
					}
					nrec.Inputs[i] = ninputs
				}
			}
			out.Records = append(out.Records, nrec)
		}
	}
	return out
}

func dedupeDescriptors(descs []ocispecs.Descriptor) []ocispecs.Descriptor {
	seen := make(map[digest.Digest]struct{}, len(descs))
	out := make([]ocispecs.Descriptor, 0, len(descs))
	for _, desc := range descs {
		if _, ok := seen[desc.Digest]; ok {
			continue
		}
		seen[desc.Digest] = struct{}{}
		out = append(out, desc)
	}
	return out
}
